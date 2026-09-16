# Bounding plan expansion for nested abstract selections

- **Date:** 2026-09-16
- **Module:** `github.com/syssam/graphql-go`
- **Status:** Approved design; not yet implemented
- **Files:** `plan.go`, `limits.go`

## 1. Background and Problem Statement

`compileSelection` (`plan.go:139`) expands an abstract parent eagerly: it
compiles one concrete selection set for every possible object type of the
interface or union. Each expanded concrete selection can contain further
abstract fields, which expand again. For an interface with `N` implementers
selected `D` levels deep, the compiler builds `N^D` selection sets.

This was measured on a schema with 12 types implementing a self-referential
`interface Node { id: ID! next: Node }`, against the query
`{ root { id next { id next { ... } } } }`:

| Depth | Compile time | selectionSets built |
|------:|-------------:|--------------------:|
| 3 | 13 ms | 24,506 |
| 4 | 138 ms | 294,074 |
| 5 | 1.65 s | 3,528,890 |
| 6 | **20.2 s** | **42,346,682** |

The query text is roughly 200 bytes. Depth 7 extrapolates to about four
minutes of single-threaded CPU and tens of millions of live heap objects,
which are then retained by the plan cache.

### 1.1 The limits do not guard against this

`WithMaxDepth` and `WithMaxComplexity` read `plan.complexity` and `plan.depth`
(`limits.go:94`), which `compilePlan` fills in only after the whole plan is
built (`plan.go:123`). `rejectIfOverLimit` therefore cannot run until the
expansion it would have prevented has already happened:

```
e := NewExecutor(s, WithMaxDepth(3), WithMaxComplexity(50))
e.Execute(ctx, &Request{Query: depth5Query})
→ rejected with "query exceeds maximum depth: 7 > 3" after 1.6529052s
```

The limit fires, but only after 3.5 million selection sets have been built. An
operator who sets a depth limit is told they are protected and is not. This is
worse than having no limit, because it suppresses the search for a real
mitigation.

### 1.2 Query cost repeats the walk on every request

`queryCostOf` (`limits.go:141`) is called from the operation interceptor chain
(`interceptor.go:136`) and depends on `oc.Variables`, so it cannot be cached on
the plan. It walks the entire plan tree **per request**. With `QueryCost`
enabled, the 42-million-node plan above is traversed once for every request
that uses it, not once at compile.

## 2. Goals and Non-Goals

**Goals**

1. Bound plan expansion so that a small query cannot produce an unbounded plan.
2. Make `WithMaxDepth` and `WithMaxComplexity` reject before the expansion
   rather than after it.
3. Keep the reported `depth` and `complexity` numbers byte-identical to today.
4. Keep the public API unchanged, in particular `Selection.Fields()`.
5. Keep plan structures immutable and lock-free on the request path.

**Non-Goals**

- Bounding total plan size as a function of document size times schema size.
  Memoization removes the exponential term; the remaining linear product is
  what the pre-compile guard in section 5 exists to bound.
- Changing when limits are enforced per request. `rejectIfOverLimit` keeps its
  call site and its semantics.
- Lazy execution-time planning. See section 3 for why it was rejected.

## 3. Approaches Considered

### 3.1 Memoized expansion, collapsing the tree to a DAG (chosen)

`compileSelection(obj, nil, sels)` is a pure function of `(obj, sels, cond)`,
and plan structures are read-only once built — the only writes to `planField`
and `fieldDef` state happen during schema construction (`object.go:265`).
Identical `(concrete type, selection set)` pairs can therefore be compiled once
and shared by pointer.

The number of distinct selection sets becomes `types × distinct selection sets
in the document` rather than `types ^ depth`. For the measured case that is on
the order of 12 × 6 = 72 selection sets instead of 42,346,682.

It changes no external contract: no locking, no API change, errors stay at
compile time, plans stay immutable, reported numbers stay the same.

### 3.2 Lazy per-concrete-type planning (rejected)

The approach taken by `ref/graphql` (`plan.go:205-240`, `abstractAlternative`):
do not expand abstract fields at compile time; plan each concrete type the
first time it is actually encountered at execute time, memoized under a mutex.

This bounds expansion by what a request actually touches, which is a tighter
bound than 3.1 and independent of schema size. It was rejected for three costs:

1. It requires a lock on a structure shared across requests, and a lookup on
   the write path.
2. `Selection.Fields()` (`context.go:241`) is public API documented as visiting
   "fields from all possible types" for abstract parents. Resolvers use it for
   projection pushdown — deciding which columns to select. Under lazy planning
   it cannot enumerate possible types without forcing the very expansion being
   avoided. Projection pushdown is most valuable exactly on polymorphic
   queries, so this is not a theoretical loss.
3. Literal argument decode errors (`plan.go:317`) would move from request
   validation to mid-response field errors, since a fragment that only applies
   to one concrete type is only compiled when that type is returned.

### 3.3 Both (rejected)

Lazy planning on top of memoization gives the tightest bound and all three
costs of 3.2. Not worth it.

### 3.4 A hard expansion budget (rejected as the primary fix)

Cap the number of selection sets a single plan may produce and reject beyond
it. Simple and unconditionally safe, but the cap is an arbitrary constant that
rejects legitimate large queries, and it treats the symptom. It remains
available as a later belt-and-braces addition; it is out of scope here.

## 4. Memoized Expansion

### 4.1 The memo table

`compiler` gains:

```go
memo   map[selKey]*selectionSet
nodeID map[ast.Selection]int32
```

The memo lives on `compiler`, which is per-compilation, so it is per
`(operation, @skip/@include variant)`. It must not be shared across variants:
`c.cond` determines which fields `included()` keeps, so the same
`(obj, sels)` pair compiles differently under different variants.

`compileSelection` consults the memo at entry and stores the result before
returning, on both the concrete (`abs == nil`) and the abstract path. `selKey`
therefore identifies the parent by whichever of the two is set — the
`*objectType` for a concrete parent, the `*abstractType` for an abstract one —
alongside the selection-set fingerprint. The two never collide because a given
type name is either an object or an abstract type, never both.

### 4.2 The key must be content-based

Keying on the selection-set slice header does not work. `plan.go:328` reuses
`first.SelectionSet` when a field group has a single AST node — a stable
pointer — but appends a fresh slice when the group has more than one:

```go
var merged ast.SelectionSet
if len(g.fields) == 1 {
    merged = first.SelectionSet
} else {
    for _, f := range g.fields {
        merged = append(merged, f.SelectionSet...)
    }
}
```

A query that duplicates a response key, `{ root { next { ... } next { ... } } }`,
therefore produces a new slice at every level, misses a pointer-keyed memo, and
reproduces the full exponential blowup. **A pointer-keyed memo is defeatable by
construction and does not fix the vulnerability.**

The key is instead built from the identity of the AST nodes in the selection
set. `ast.Selection` is an interface holding a pointer to `*ast.Field`,
`*ast.FragmentSpread` or `*ast.InlineFragment`; those addresses are stable for
the lifetime of a cached document, and interface values holding pointers are
comparable, so they can be used as map keys directly. `compiler.nodeID` assigns
a dense `int32` to each node on first sight (amortized O(1), no upfront
document walk). `selKey` combines the object type with the sequence of those
ids.

No `unsafe` and no `uintptr`: the root package's constraints hold, and pointer
identity is expressed through the comparable interface value rather than its
address.

### 4.3 Metric walks must memoize too

Three walks recurse over the plan and would re-walk shared subgraphs
exponentially once the tree becomes a DAG:

| Walk | Location | Frequency |
|---|---|---|
| `complexityOf` | `plan.go:365` | once per compile |
| `depthOf` | `limits.go:220` | once per compile |
| `queryCostOf` | `limits.go:141` | **once per request** |

Each gains a `map[*selectionSet]int` memo. For `complexityOf` and `depthOf` the
memo is per compile. For `queryCostOf` the memo is created per call: `vars` and
`cfg` are fixed within one request, so memoizing by `*selectionSet` pointer is
sound within a call and must not outlive it.

`queryCostOf` is the one that matters for throughput. It is already the
per-request cost of the current design (section 1.2); the memo is what makes
the DAG safe *and* fixes a pre-existing per-request cost.

The interleaved benchstat run that established "no hot-path regression" for
this change covered `BenchmarkExecuteConcurrentList`, `BenchmarkExecuteUsers`,
`BenchmarkLazySeq*` and `BenchmarkListResults*` — none of which configures
`WithQueryCost`, so none of them exercises this new per-call
`map[*selectionSet]int` allocation. The cost-disabled path is genuinely
untouched (`rejectIfOverLimit` only calls `ensureCost` when `cost.Max > 0`,
and `attachCost` returns early when `Report` is false), so a regression here
is unlikely — but that is an argument from the code, not a measurement. The
cost-enabled path, which is the one this section changed, is unmeasured.

## 5. The Pre-Compile Guard

### 5.1 One implementation, moved to the AST

The guard must run before `compilePlan`. The alternative of keeping the
plan-tree walks for reporting and adding a second AST-based walk for admission
was considered and rejected: `complexityOf` and `depthOf` take a max over
concrete types for abstract parents, merge duplicate response keys by alias,
and apply the `@skip`/`@include` variant. Each is a place two implementations
can disagree, and disagreement means an error message that contradicts the
number reported in `extensions`.

Instead the existing walk is **moved** to the document AST and runs before
compilation. It expands fragment spreads, groups by type condition and takes
the max for abstract parents, dedupes by response key, and applies the same
`cond` map — reproducing today's numbers exactly. This is a hard requirement,
not an aspiration; section 6 pins it.

The old plan-tree implementations are not deleted. They move to a test file as
the reference oracle for the differential fuzz test.

### 5.1a What the guard is worth once the memo exists

Measured during implementation, and worth recording because it contradicts the
motivating number in section 1.1: once section 4's memoization has landed,
`WithMaxDepth(3)` against a depth-8 fan-out rejects in roughly 500-700
microseconds **whether or not the guard is present**. The 1.65-second figure was
taken before memoization. On this shape the memo alone already bounds
compilation, so the guard buys no measurable latency.

That does not make the guard redundant, but it does mean its justification has to
be stated honestly:

- A rejected query builds and caches **no plan at all**. Without the guard, every
  query that will always be refused still compiles a plan and occupies a plan
  cache entry.
- Compilation is bounded by *document size × type count*. Memoization removes the
  exponential term but not that product, and it is the only remaining way a
  single request can make compilation expensive. This is what the guard is for,
  and the fan-out fixture does not exercise it.
- The limit becomes a precondition rather than a postcondition.

A test that asserts the guard by timing is therefore measuring nothing after
section 4 lands. The property to assert is structural: after a rejection, no plan
exists for that operation and variant.

### 5.2 Where it runs

`docEntry.planFor` (`plan.go:447`) runs the walk before `compilePlan` and
returns the limit error without compiling when it is exceeded. The
uncacheable path, `docEntry.planUncacheable()` (`len(d.condVars) >
maxCondVars`, which compiles without caching because there are too many
@skip/@include variables for the variant cache), gets the same treatment.

The results fill `plan.complexity` and `plan.depth`, so:

- `rejectIfOverLimit` keeps its call site (`interceptor.go:136`), keeps reading
  the numbers off the cached plan, and keeps running per request. Enforcement
  semantics are unchanged for callers.
- The guard costs nothing on a plan cache hit. It runs only on the compile path
  it exists to protect.
- On the uncacheable path, every request already recompiles the plan, so the
  guard adds one full `operationMetrics` walk per request there — the one path
  where the new walk is genuinely per-request rather than per distinct
  document. Bounded by document size × type count, so not a risk, but worth
  naming: it is not free the way the cache-hit case is.

### 5.3 Interaction with query cost

`QueryCost.Max` is enforced in `rejectIfOverLimit` from `oc.costValue`, which
`attachCost` computes per request from variables. It is not moved: it depends
on request variables, so it cannot be computed at compile time, and with the
memo of section 4.3 its walk is bounded. Cost is not a compile-time guard and
this design does not make it one.

## 6. Testing

`sh scripts/gate.sh` is the gate. Specific tests this change requires:

| Test | Assertion |
|---|---|
| Fan-out regression | 12 types × depth 6 compiles within a bounded time and node count (today: 20.2 s, 42.3M nodes) |
| **Deliberate break** | With the memo removed, the fan-out regression **must fail**. A test that still passes against a broken memo is agreeing with the code, not checking it |
| Memo defeat vector | `{ root { next { ... } next { ... } } }` — duplicate response keys, which produce fresh merged slices at every level, must still hit the memo |
| Guard | After a `WithMaxDepth` rejection, no plan was compiled or cached for that operation and variant. **Not** a timing assertion: once section 4 lands, the rejection is sub-millisecond with or without the guard, so timing it measures nothing (section 5.1a) |
| Differential fuzz | The AST walk and the retained plan-tree oracle agree on `depth` and `complexity` for fuzzed documents |
| `-race` | Concurrent compiles of one document, and concurrent reads of shared `selectionSet` pointers across executor goroutines |
| Cost memo | A fan-out query under `WithQueryCost` has bounded per-request time |

The deliberate-break row is the load-bearing one. Per `CLAUDE.md`, a fan-out
benchmark on this codebase once reported 45 ns per subscriber for events that
reached nobody, and a subscription leak test passed against a deliberately
broken release path. The regression test is only evidence if it fails when the
memo is gone.

## 7. Observable Changes

- **Duplicate compile errors collapse only where a memo key actually repeats.**
  An earlier draft of this section claimed that a bad literal argument under an
  abstract parent, currently reported once per concrete type, would collapse to
  one report. That is wrong, and `plan_duperr_test.go` measured it: the memo is
  keyed on `(parent, selection set)`, and the abstract branch compiles each
  implementer with the *same* selection set but a *different* `*objectType`, so
  every implementer is a distinct key. Three implementers still produce three
  errors.

  The collapse is real only where one `(parent, selection set)` pair recurs —
  which needs the same concrete type at two levels of the same query, not a
  single fan-out. The characterisation test keeps its assertion of three and
  gains a better purpose than the one it was written for: it now pins that the
  memo key includes the parent type, so a later "simplification" that keyed on
  the selection set alone would fail it.
- **`Selection` values may share pointers.** Not user-visible: plan structures
  are read-only at runtime (section 3.1).
- **A guard rejection bypasses the operation interceptor chain entirely, not
  just `extensions.cost`.** Before this change, a depth/complexity rejection
  happened inside `rejectIfOverLimit`, itself called from within `e.opChain`
  (`interceptor.go:136`) — so it ran wrapped by every registered
  `OperationInterceptor`, with a real `OperationContext` in hand. Now
  `docEntry.compile` rejects in `execute` (`exec.go:194-196`) before an
  `OperationContext` exists, and `execute` returns `e.requestError(...)`
  directly without ever calling `e.opChain`. Concretely for `ext/otel`: the
  span is never renamed from `graphql.request` to `query Foo`;
  `AttrOperationType`, `AttrOperationName`, `AttrCacheHit`, `AttrComplexity`
  and `AttrDepth` are never set; the duration/error metric falls back to the
  request layer, losing its operation dimensions. `extensions.cost`
  (`attachCost`, `limits.go:112`, requires `oc.plan`) is one symptom of this —
  today a query rejected for depth or complexity still has a compiled plan
  and an `OperationContext`, so cost is reported alongside the rejection;
  once the guard rejects first there is neither, so there is no cost to
  report. This applies only to queries rejected by `WithMaxDepth` or
  `WithMaxComplexity` — queries rejected by `QueryCost.Max` are unaffected,
  since that check stays in `rejectIfOverLimit`, after compilation, because it
  depends on request variables.

  The same bypass means a guard-rejected query never reaches any operation
  interceptor, including a rate limiter: `main` has since gained an
  `ext/throttle` built as exactly such an interceptor, so a rejected query is
  unmetered by it. Scope this honestly rather than alarmingly: the guard
  rejects before `compilePlan`, so an unmetered rejected request costs parse +
  validate + one memoized `operationMetrics` walk over the AST — not a plan
  compile — and that walk is bounded by document size × type count. Before
  the guard existed, the same abusive query would have compiled, built an
  `OperationContext`, and been both charged and metered; now it is refused
  earlier and cheaper, but outside every interceptor's view.
- **Subscriptions: an over-limit subscription is now rejected once, at
  `Subscribe`, instead of once per event.** `subscription.go:146-149` makes
  `Subscribe` return a `*SubscribeError` when the guard fires, before the
  stream ever opens. Previously `Subscribe` succeeded, the stream opened, and
  `rejectIfOverLimit` ran inside `opChain` on every event — an over-depth
  subscription emitted an `error` `next` payload forever, once per event,
  until the client gave up or disconnected. The new behaviour is strictly
  better for the client and the server, but it changes what `gqlws`/`gqlsse`
  put on the wire: a single `error` message followed by a close, rather than
  an indefinite stream of per-event error payloads.
- **Plan memory for polymorphic queries drops sharply.** The plan cache holds
  documents, so this also reduces steady-state RSS on schemas with wide
  interfaces.
- **No change to reported `depth`, `complexity` or `cost` values**, by
  construction and pinned by the differential fuzz test.

## 8. Out of Scope

Findings from the same review that are deliberately not addressed here, in the
order they should be considered afterwards:

1. No query-size cap on the plan cache. `planCache.put` stores any query;
   default 1024 entries × default 1 MiB body is roughly 1 GiB of retained query
   text before counting parsed ASTs. `ref/graphql` bypasses its cache above
   `MaxQueryBytes` (64 KiB default).
2. No response size cap and no operation timeout.
3. Validation failures are not cached (`exec.go:130`), and concurrent misses on
   one query are not deduplicated — parse and validate are outside `d.mu`, so
   N concurrent requests for a new query do the work N times.
4. A `stats.Handler`-style passive field hook, so `ext/otel` can record
   per-field timing without a field interceptor forcing every field, pure ones
   included, through the type-erased path.
