---
paths:
  - "plan.go"
  - "plan_*.go"
  - "limits.go"
  - "interceptor.go"
  - "validate.go"
  - "request.go"
---

# Plan compile, limits and cost

**Parsing and validation are bounded before they run** (`parseDocument`, `document_limits.go`,
`overlap.go`): a token limit, a nesting limit, a fragment-lookup budget, and gqlparser's
OverlappingFieldsCanBeMerged swapped for a linear FieldsInSetCanMerge above 256 selections.
They exist because validation runs before every depth, complexity and cost limit, and
gqlparser's default rules made a 10-60 KB request cost seconds and gigabytes (numbers in
`docs/operations.md`, "Document limits"). **The replacement rule must agree with gqlparser's on
validity**, not wording: `TestFieldsCanMergeAgreesWithGqlparserSpec` runs graphql-js's cases from
gqlparser's module, `TestFieldsCanMergeAgreesWithGqlparserRandom` 20 000 generated documents, and
`FuzzFieldsCanMerge` the same differential. gqlparser compares untyped fields by name and
treats a composite type as conflicting with nothing, so its relation is not transitive --
`shapeConflict` compares leaf types with the first leaf, not the first field, for that reason.
The fragment budget was written as a stopgap for the walker's linear `Fragments.ForName`, to be
dropped when gqlparser indexed fragments. v2.5.59 does, and **the budget is still needed**:
measured with it removed, the largest fragment DAG the token limit admits (1 362 fragments,
58 KB) validates in 2.3 s under the large-document rules, because the rules still walk a
fragment once per spread that reaches it. The count measures that too, so it stays. The
linear merge rule is still needed as well: on the same release, 1 249 fragments spread once
validate in 1.47 s under gqlparser's rule and 5 ms under the replacement. **Agreement includes list nullability**: gqlparser steps into a
list without comparing the list's own non-null, the replacement compared it, and `[Int]!` against
`[Int]` under one response name was valid until a document grew past 256 selections
(`TestMergeValidityDoesNotDependOnDocumentSize`). The differential's schema had no `[T]!` field.

**Plan compile (`plan.go`).** A document is parsed and validated once and
cached in an LRU (`docEntry`), bounded by entry count and by query text
(`WithPlanCacheBytes`, 16 MiB by default). The byte budget matters more than it looks: a
parsed and validated document retained roughly 26 times its query text, measured linear from
64 KiB to 1 MiB on an alias-heavy query (other shapes will differ), so without it 1024 distinct
valid 1 MiB queries of that shape would hold tens of gigabytes. The
document is cached before the depth and complexity guard runs, so that guard does not bound
this. **Concurrent misses for one query text share a single parse** (`Executor.document`,
`docCall`): a 100 KB query measured about 8 ms and 6.8 MB to parse and validate, and 64 identical
requests on a cold cache took 19 times the wall time of one. Plan compilation already had this
property through `docEntry.mu`; parsing did not. Only a success is cached — a failure is shared
with requests already waiting and no further, so distinct invalid queries cannot evict valid
documents, which is why caching validation failures was declined. **The leader keeps its errors
and stores clones for the waiters**, who clone again: the first version handed waiters the
leader's own `*Error`s to clone while the leader's presenter was annotating them, a race only a
presenter that mutates its argument hits (`DefaultErrorPresenter` copies first) and only `-race`
shows (`TestDocumentFlightPresenterMayMutate`). `Error.clone` copies `Path` and `Locations` as
well, since an append on a shallow copy writes into the other's array. A waiter gets an internal
error rather than a nil entry if the parse panicked. `docMu` is one lock for
the executor, and a miss hashes the query text under it -- which this file called "known and
accepted" for a long time without measuring. Measured now (`BenchmarkDocumentMiss`, every
iteration a distinct query so every one misses): 841.8us/op at `-cpu 1`, 326.0 at 2, 116.8 at
4, 71.3 at 8, 60.8 at 16 -- 13.8x on 16 cores, still climbing, with allocations constant at
2 578/op. **Misses scale; the lock is not the bottleneck**, because it covers only the hash
and the map lookup while parse, validate and compile happen outside it. The pessimistic note
was wrong and the number replaces it; re-measure before believing either. The tests hold the parse open with `testHookParseDocument` inside `testing/synctest`,
so "concurrent" is exact. Each `(operation, @skip/@include variant)` compiles to an
immutable `plan` with fragments flattened, directives constant-folded per variant (up to
`maxCondVars` boolean variables), arguments pre-decoded and response keys pre-serialized.
**Variants are bounded twice, because a client chooses them**: sixteen conditional variables
are 65 536 plans of one query text, every one used to be kept while its document stayed
cached, and the budget counted the text once -- 2 048 variants of a 17.8 KB query held 1.1 GB
under an entry the cache reported as 17.8 KB. A document keeps at most `maxPlanVariants` (64)
and each plan after its first is charged the query text again (`planCache.charge`), evicting
other documents to make room; past either bound the variant is compiled for its request and
not kept (`plan_variants_test.go`). **Pre-decoded means scalars and enums**: a list or
input-object literal (or default) is validated at compile and decoded per request
(`literalAggregates`), because decoded once it is one slice or struct shared by every request,
and a resolver that sorted or defaulted its own arguments changed the next request's
(`TestLiteralArgumentsAreNotSharedBetweenRequests`). A custom scalar that decodes a scalar
literal to something holding a slice is still shared. **This is a trade and it costs**:
`BenchmarkExecuteLiteralAggregateArgs`, a field given a three-element list and a two-field input
object as literals, went from 511 ns to 1 413 ns a request, 560 B to 1 505 B and 7 allocations to
24 (interleaved n=12, p=0.000), with `ExecuteConstantArgs`, `ExecuteVariableArgs`,
`ExecuteUsers` and `ExecuteTypenameHeavy` not distinguishable. It is what the same query costs
when it sends those arguments as variables, which is how a client that parameterises its
queries sends them anyway. The alternative is to decode once and document that a resolver must
not modify its arguments.
The variant store also changed shape after a review of this change: a variant that will not be
kept is compiled outside `docEntry.mu` (`TestAnUncachedVariantIsNotCompiledUnderTheDocumentLock`,
which looks at the lock from inside a compile), since it is compiled again on every request for
it and under the lock held up every other request for the document.
`selectionSet` carries `byType` for abstract parents plus the scheduling counts the
executor needs. Abstract parents expand through a memo keyed on `(parent type, selection
set)`, so the expansion is a DAG rather than a tree — without it, 8 implementers selected 6
deep built 2,696,338 selection sets; with it, 64. The memo key is a fingerprint of AST node
identities, not of the selection slice: `buildField` allocates a fresh slice when a response
key has more than one AST node, so a pointer key is defeated by `{ a { b } a { b } }`. Every
walk over a plan must memoize on `*selectionSet` for the same reason, `queryCostOf` included,
because it runs per request. Depth and complexity are computed by `operationMetrics` from the
document before `compilePlan`, so the limits refuse a query rather than reporting on one
already expanded; `plan_metrics_oracle_test.go` holds the plan-tree walk it replaced, and the
two are checked against each other by `TestOperationMetricsMatchesPlan` and fuzzed by
`FuzzOperationMetrics`. The guard's value is structural, not latency: with memoization already
in place, a rejected query and a compiled one finish in the same sub-millisecond range, since
the memo bounds compilation whether or not the guard runs first — what the guard actually
buys is that no plan is built or cached for a rejected query, and that compilation stays
bounded by document size times type count, which memoization alone does not bound. The guard
rejects from `execute`/`Subscribe` directly, before an `OperationContext` exists, so a
rejected query never reaches `opChain` or any `OperationInterceptor` — `ext/otel` never
renames its span or sets the operation attributes, and `ext/throttle` never sees the request
to meter it. That is one full `operationMetrics` walk over the AST, not a plan compile, so
the request is cheaper than before the guard existed, just invisible to interceptors; the one
path where the walk runs on every request rather than once per cached document is
`docEntry.planUncacheable()`, where every request already recompiles anyway. A subscription
over the limit is rejected once at `Subscribe`, before the stream opens, instead of emitting
an error `next` on every event as it did before this guard existed. Cost depends on the
variables, so `Subscribe` refuses it separately (`rejectIfOverCost`) after coercing them and
still before the source opens (`TestSubscribeRefusesAnOverCostOperationBeforeTheSourceOpens`);
each event's `OperationContext` inherits the base's computed cost, or `oc.Cost()` in an event
interceptor is the no-model fallback (`TestSubscriptionEventCostUsesTheConfiguredModel`).
**The page size is read in every spelling `Int` accepts**: `asCostInt` falls back to
`Float64` for `5e2` and `500.0`, which `rawInt64` coerces, or such a page is priced as
`DefaultListSize` while the resolver receives the real one
(`TestAFloatSpelledPageSizeCostsTheSameAsAnInteger`).

**A variable's default is validated once per document** (`variableDefaultErrors`, in
`parseDocument`). gqlparser checks a default's kind and not its value, so `$l: Int = 99999999999`
ran the operation, sibling resolvers included, and failed whichever field used it, where the same
literal as an argument is a validation error. **A list under a paid connection field is priced by
its own page size when it names one** (`fieldCost`): it is then a second page rather than the
connection's edges, and `items(first: 500)` used to cost what `items(first: 5)` did.

**Operation interceptors wrap the engine's own limit check; they do not follow it.**
`rejectIfOverLimit` and `attachCost` are the innermost layer of `opChain`
(`interceptor.go:132`), and an interceptor registered earlier sits further out. So an
extension reading `OperationContext.Cost` runs before the engine would compute it, which is
why `execute` computes it eagerly whenever a cost model is configured, and why a limiter must
be registered before any interceptor that should not run for a request it rejects.

**Actual query cost (`limits.go`).** `QueryCost.Actual` sums the weight of every field
really resolved, alongside the requested cost computed from assumed list sizes; the two
together say whether `DefaultListSize` is near reality. Weights are resolved onto
`planField.costWeight` at plan compile, so the write path adds an integer rather than
looking up a coordinate, and the weight is zero unless the feature is on.

`QueryCost.Connections` prices a Relay connection by its `first`/`last`, which sit on the
connection field one level above the `edges` list they bound and are otherwise invisible:
without it `conn(first: 2)` and `conn(first: 200)` both cost `DefaultListSize`. The page size
pays for the list directly inside it and nothing deeper, or the two multiply and a page of
200 prices as one of 2000. Off by default: it changes the number an existing deployment set
`Max` against. **Cost arithmetic saturates** (`costAdd`, `costMul`): the client picks the
page sizes, and three nested `first: 2147483647` lists wrapped to a negative cost that passed
any `Max` and credited `ext/throttle`. **Complexity saturates too** (`plan_metrics.go`): the walk is linear in the
document but measures a tree that can double per level, and 64 fragments each selecting a
self-referencing field under two aliases wrapped it to -1 -- a 5 KB request under any
`WithMaxComplexity` that held gigabytes (`TestComplexitySaturatesInsteadOfWrapping`). Any new
sum over the walk goes through `costAdd`. The walk carries its state on `costWalk`, and **a memo over it must key on
`paid` as well as the selection set** — the same `*selectionSet` costs differently paid and
unpaid.

## Field arguments are read off the AST, never through `ArgumentMap`

`fieldArguments` walks `f.Definition.Arguments` itself and evaluates each one with `astJSON`.
It used to call `ast.Field.ArgumentMap(vars)` and normalise the result with `asJSON`, which
routes every literal through gqlparser's `Value`, and that has two properties a request path
cannot have.

**It panics on an integer literal wider than int64** -- `strconv.ParseInt: value out of
range`, raised inside `arg2map`. Validation does not save you: `Int` and `Float` reject an
oversized literal, but **a custom scalar accepts anything by definition**, so
`{ f(v: 123456789012345678901234567890) }` against any schema declaring a custom scalar
argument is a client-supplied panic. It escapes `Execute` -- `WithRecover` guards resolvers,
not argument coercion -- and it is reachable from plan compile (`plan.go`) as well as from
execution and both subscription paths. Any schema with a `Decimal`, `JSON`, `Any` or `Cursor`
scalar taking an argument had it; the real consumer schema declares ten.

**And it rounds a float literal through float64**, so `9007199254740993.0` reached a custom
scalar as `9.007199254740992e+15` when inlined and exactly as written when sent as a variable,
as did `1e3` (`"1000"` against `"1e3"`) and `1.0` (`"1"` against `"1.0"`). `astJSON`'s own doc
says literals and variables must produce the same representation so custom scalars see one
form; they did not, and a `Decimal` decoded a different number depending on whether the client
parameterised the query. `astJSON` keeps `json.Number(v.Raw)`, so the text survives and both
failures are unreachable.

The replacement is `arg2map`'s logic verbatim except for that substitution and returning the
error instead of panicking: a supplied argument wins, a variable absent from `vars` leaves the
argument unset so its default applies, everything else falls back to the definition's default.
All four call sites already handled a decode error, so the error joins that path.

It costs nothing. Interleaved n=12 over `ExecuteConstantArgs`, `ExecuteVariableArgs` and
`PlanCompileWithConstantArgs` -- benchmarks added with this change, because **every engine
benchmark uses argument-free fields, so measuring one against them measures nothing**:
allocations identical sample for sample on all three, constant-argument and plan-compile time
not distinguishable, and the variable path **-5.11% (p=0.003)**, since it no longer builds a
Go value only for `asJSON` to rewrite it.

`TestACustomScalarSeesLiteralsAndVariablesAlike` drives both failures across eleven shapes.
Reverting `fieldArguments` to `ArgumentMap` fails it on the exponent, trailing-zero and
precision cases and panics the binary on the wide-integer one.

**Introspection is not charged against depth, complexity or cost, and gqlparser's
`MaxIntrospectionDepth` is replaced.** `examples/veloxfx`, configured the way the docs
recommend (`WithMaxDepth(10)`), refused the introspection query GraphiQL and every client
generator send with `MAX_DEPTH_EXCEEDED`: its `TypeRef` fragment nests `ofType` seven deep. On a
300-entity schema that query is also 4.3 MB and, priced by list sizes, costlier than any
budget an API would set for its own queries. So `__schema` and `__type` count as one field in
`metricWalker` (and the plan oracle, which must agree), are free in `fieldCost`, and carry no
`costWeight` -- nor does any field of a `__` meta type, which only introspection reaches.
What bounds introspection instead is graphql-js's rule: a path may pass through at most two of
`fields`, `interfaces`, `possibleTypes`, `inputFields`. **gqlparser ships that rule in its
default set, and until v2.5.59 without a memo**: it re-walked every path through fragment
spreads, so 24 fragments each spreading the next twice -- 1.1 KB -- cost 2.0 s of validation,
doubling per fragment, with introspection disabled too. This repository replaced it with a
memoized copy for as long as that was true. v2.5.59 memoizes it upstream, every introspection
test passes on gqlparser's own rule, and the copy is gone; with introspection disabled the rule
is still removed, since `NoIntrospection` refuses anyway.
`TestMaxIntrospectionDepthIsLinearInFragments` (41 fragments, 10 s budget) now guards the
dependency rather than a copy of it.
