---
paths:
  - "authz*.go"
  - "examples/storefront/**"
---

# Authorization

**Authorization is compiled, not wrapped.** `AuthShape` is built once at plan compile
(`buildAuthShape`) and cached with the plan; it does not depend on the principal, so
`planKey` is unaffected and the plan cache is not multiplied by policy. A field that
declares `@requiresScopes` gets `planField.authIdx >= 0` at construction; every other field
gets `-1`, so the ordinary request path pays one integer compare and no allocation.
Argument sites declared with `@authorizeInput` route through that same compare: a field
with at least one gets a zero-requirement output site, when it declares no requirement of its
own, so `authIdx >= 0` still routes it, and a field with none pays nothing. The per-site input walk (`Decision.Input`) reads the
operation's AST plus its variables, never `ArgumentMap`, because `ArgumentMap` fills in SDL
defaults the client did not choose. **A subscription's source is opened with arguments
decoded again from `oc.Variables`**, the map the Authorizer walked, not the ones decoded
before the interceptor chain: `Variables` is writable, and an interceptor rewriting it
showed the policy one input while the source opened with another.
`execState` (64 bytes) and `OperationContext` (208 bytes, with its 48-byte
`WaveCoordinator` by value) hold those sizes (`TestStructSizes`,
`authz_bench_test.go`) — re-measure both, interleaved, before adding a field to either; see the `execState`/`OperationContext` entry in `executor.md` for why a
non-interleaved reading is not evidence. Effective requirements — a field's own
`@requiresScopes` ANDed with its object type's and with each implemented interface's
type-level and same-named-field requirement — are resolved once, in
`resolveAuthRequirements` (`authz_shape.go`), and stored on `fieldDef.requires` /
`objectType.requires`; `shapeBuilder` and `validateAuthCoverage` both read those stored
values, never recomputing them. **Never compute a requirement from `requirementOf` directly
at plan compile or in coverage** — that bypasses the one place the cap and the
interface-combination logic live, and is how enforcement and coverage would silently
diverge. The unguarded `__typename` fast path in `writeFieldValue` (`exec_object.go`) —
returning `obj.name` before `execState` is even touched — is what keeps the measured cost
of this at about 1%.

**Which SDL directive yields a `Requirement` is declared, not hardcoded.**
`RequirementDirective(name, arg, shape)` adds a spelling; `@requiresScopes` is seeded as a
built-in entry before options apply, so it always works and redeclaring it collides. The
option *adds*, and two spellings on one coordinate AND — the rule repeated occurrences of one
directive already follow, so more declarations never mean less restrictive. `ScopeShape` has
no valid zero value on purpose: `@auth(requires: ["a","b"])` is the same text whether the
author meant AND or OR, so reading a flat list as AND when they meant OR silently widens
access and the SDL cannot say which was intended.

`MarkerDirective(name, scope)` covers the other shape Apollo uses: a directive with no
argument at all, `@authenticated`, whose presence alone is a requirement for one scope the
caller names. It is a `reqDirective` entry like any other, so inheritance, the cap,
misplacement rejection and ANDing with a scope directive all come free -- breaking either the
reading or the misplacement half fails `TestMarkerDirectiveIsEnforced` or
`TestMarkerMisplacementIsRejected`. **Apollo's `@policy` needs nothing**: it is
`[[String!]!]` like `@requiresScopes`, so `RequirementDirective("policy", "policies",
ScopesNested)` enforces it today and the namespace is the Authorizer's business. That is most
of what `ext/authz` was scoped to be, which is why the package is still unbuilt rather than
overdue.

**The spelling is welded into four places and they move together**: reading
(`requirementOf`/`groupsOf`), the literal type-check (`checkRequirementDirectives` — gqlparser
never type-checks a directive argument's literal), misplacement rejection (`rejectUnenforced`)
and the error wording. Reading a directive without also rejecting its misplacement is the
fail-open the design exists to prevent: `@auth` on a schema definition would be silently
ignored while the author believes a requirement is in force. Breaking each of the three fails
`TestCustomDirectiveMalformedLiteralIsRejected`,
`TestCustomDirectiveMisplacementIsRejected` and `TestCustomDirectiveIsCapped` respectively.
Two things that cost time to learn: the built-in entry must skip the "is this directive
declared in the SDL" check, because almost no schema declares `@requiresScopes` and requiring
it broke every existing schema; and the cap is on *group count*, which AND multiplies, so
single-group occurrences never overflow it however many there are — a cap test needs
occurrences contributing more than one group each.

**`build()` accumulates errors and keeps going, so `groupsOf` reads literals
`checkRequirementDirectives` has already condemned** — confirmed by making `groupsOf` panic
and watching a rejected `[[1]]` reach it. The reader must therefore be panic-free on input
the validator refused, not merely on input it accepted, which is what
`FuzzRequirementDirectiveLiteral` holds: 172k executions over the three shapes found nothing,
and it also fails a schema that builds carrying an empty requirement group, since `Satisfied`
over one is vacuously true and would read as guarded while guarding nothing. It is the only fuzz
target whose *input* reaches `NewSchema`: `FuzzExecute` and `FuzzOperationMetrics` each build
their schema once in setup and fuzz a query against it, so no generated byte has ever reached
the schema builder before this. CI discovers targets with `go test -list` over `go list ./...`, so a new
one needs no workflow edit.

**`ScopeShape` is a public named type, so every value of it is constructible** — including
`scopesMarker`, which is unexported only by name. `RequirementDirective` reached it and
silently reinterpreted its `arg` from an SDL argument name to a scope: the same parameter
meaning something else, which is the failure this option set exists to prevent. `reqDirective`
now carries `viaMarker` so the validator can tell which constructor made the entry, and
`RequirementDirective` refuses the shape by name. An unexported constant in an exported
integer type hides nothing; only a check does.

**Instance sites (`@authorizeObject`, `authz_instance.go`) are decided during execution, not
in the `Decision`** — the values do not exist when a `Decision` is built. A marked object type
sets `objectType.instanceGuarded`; a field that can return one gets `instanceSiteBit` in
`planField.argSites` (the bit rides above the argument count because `planField` is exactly
full at 176 bytes). **The bit is the whole routing mechanism**: `writeList` and
`writeComposite` check `st.e.objectAuthorizer != nil && f.hasInstanceSite()` and never reach
an instance site through `authIdx`, so the shape stores one purely so an `Authorizer` can see
that instance checks will happen. It therefore synthesizes no output site, `Decision.Set`
refuses it, and `AuthShape.IsEmpty` counts only `decidable` sites — otherwise a plan selecting
a guarded type and declaring nothing else called a configured `Authorizer` once per request
for a decision that could change nothing. **Batching is per list, not per wave**: the list is
drained, decided in one `AuthorizeObjects` call (split by `WithObjectAuthBatch`, 50 by
default), and only then written, while a guarded object behind a non-list field is one call of
one check — an N+1 the godoc now states rather than promises away, and P10 in the design
spec records why it is not fixed by writing breadth-first: `async_graphql::Guard::check`
returns `Result<()>` with no per-instance identity to batch, so per-list batching already
expresses more than the peers can, and no reference implementation reorders traversal for
this. A batch that fails at all
fails every outstanding check, so a policy backend that is down cannot be why a row becomes
visible.

**The batching was designed for a remote policy and, until `authz_scale_test.go`, had only
ever been tested against a pure function with three rows** — no latency, no failure, and
batching that costs nothing whether or not it happens. Given a backend shaped like a real one,
four numbers, all exact rather than approximate:

- 1000 rows at the default batch of 50 is **20 calls carrying 1000 checks**, none wider than
  the batch, and 1 call at `WithObjectAuthBatch(1000)`.
- **The batches are sequential, so policy latency multiplies by their number**: 20 × 5ms =
  **100ms** added to one field, measured under `testing/synctest` so the figure is exact.
  Halving the batch doubles it. `WithObjectAuthBatch` should therefore be sized against what
  one call costs in latency, not against what the backend accepts in one request — the godoc
  now says so, because "split into sequential calls" did not.
- The nested-single-object N+1 is **2 + 100 calls** for 100 rows each reaching one guarded
  child, every one of the 100 carrying a single check, so the batch size cannot help there.
- A failing backend stops the run: **3 calls of a possible 20**, not 20. Making the failure
  path continue and fill in zero outcomes leaks every row it had not reached, which is what
  `TestObjectAuthFailsClosedFromTheMiddleOfAList` fails on.

**The `Authorizer` is linear in the sites one query touches, and flat in schema size.**
`BenchmarkAuthorizerWide1589` reaches the real consumer's site count: 8 allocs/op at 16, 64
and 256 sites and 9-11 at 1589, because the per-site cost is one `Decision.outcomes` slice —
84 KB at 1589, about 53 bytes a site. Time is ~2.4µs, 8.4µs, 33µs and 109-213µs, so roughly
linear, but the last figure moved 2x across three runs on a warm machine and only the
allocation column should be quoted. Note what the benchmark is: **one query selecting all
1589 guarded fields**, the pathological shape, not the consumer's ordinary traffic. The
`Decision` is sized from the plan's shape, so an ordinary query on a 5455-type schema carries
the sites it selected and not the ones the schema declares. Two things that only a deliberate break finds: **`Drop` must be removed before
its tasks are announced** (announcing a task that never begins strands every parked `Load` and the request
hangs to its deadline — `TestInstanceDropDoesNotStrandTheWave`, in `loader/`), and
**`writeListGuarded` must stop at the first fatal failure** the way `writeList` does, or a
denied non-null list keeps resolving elements into a buffer that is about to be rewound and
appends one error per element, every one of them carrying the first element's index. `Null` is
rejected at a non-null position by `valueNonNull` on the site, since an instance site has no
`Field` for the ordinary guard to read. **`AuthSite.ListElement` exists for the same reason**:
an `ObjectAuthorizer` has to choose between `Drop` and `Deny` for a row it withholds, `Drop` is
valid only at a list element position, and with `Field` nil there was nothing to read it from —
so a policy could only choose by knowing the schema by heart, which a policy written against a
growing schema does not. Every existing test hardcoded `Drop()` for a query it knew, so nothing
noticed; `examples/storefront` is what found it, which is the argument for an example that has
to work rather than one that reads well. The site is built by `instanceSiteOf` (`exec_object.go`)
for both the shape builder and the executor, because when they were two copies `ListElement`
was added to one and every policy still saw `false`. The bool fits the padding
`AuthSite` already had: still 136 bytes. `writeListConcurrent` and `writeListConcurrentPlain`
are a deliberate near-copy: merging them measured +20% time and +76% B/op on
`BenchmarkExecuteConcurrentList`, and `TestConcurrentListPathsAgree` pins what they write —
but not every branch, and the comment on them says which.

Two facts only exist because authorization and bounded plan expansion landed together.
compileSelection's memo makes one `*selectionSet` reachable from several parents, so the
`seen` set in `shapeBuilder.walk` is load-bearing — it keeps the walk linear in the DAG —
and sharing does not conflate decisions, because a site's requirement is read off the field
definition alone. And a Redact outcome runs the resolver, so `callLeafRedacted` carries its
own copy of `callLeaf`'s `FieldObserver` handling, defer order included; Deny, Null and Zero
never resolve and are never observed. Drop that copy and a redacted field is invisible to
`ext/otel`'s field spans while every other test stays green — `TestOutcomeRedactIsObserved`
is the one that notices.

**`AuthSite` is 136 bytes where 120 would fit the next size class down, and that was
measured and left alone.** `fieldalignment` reports the 11% waste; the three small fields
(`Kind`, `leaf`, `valueNonNull`) sit apart because the struct is grouped by meaning and
godoc shows exported fields in declaration order. Before reordering it, note what the
measurement said about the larger version of the same cost: `ScopeAuthorizer` and the
argument-input walk used to range over `shape.sites` *by value*, copying all 136 bytes per
site per request, and converting both to indexing (which is the right idiom and is what
`gocritic`'s `rangeValCopy` asks for) moved nothing — interleaved n=12 over
`BenchmarkAuthorizerWide16/64/256`, p=0.434, p=0.164 and p=0.630, allocations equal sample
for sample. If copying the whole struct per site is invisible at 256 sites, 16 bytes of
padding inside it is not the thing to spend a layout change on. Those benchmarks exist
because `BenchmarkExecuteWithAuthorizer` has two sites, which is too few to show any
per-site cost at all; `TestWideAuthzSchemaHasASitePerField` keeps them honest by pinning
that the fixture really does produce one site per field.

## What a line-by-line review of `authz_shape.go` found

The file is 820 lines and was at 93.5% with four uncovered blocks, so coverage was not what
surfaced any of this. What surfaced it was breaking each claim a comment makes and seeing
whether anything failed.

**Site order is the Authorizer's contract, and the sort that fixes it had no test.** `walk`
sorts the possible-type names before descending "so a document's site indices do not depend on
map order". Deleting the sort leaves every test in the repository green, five runs in a row.
It is not an internal detail: `Decision.Set(site int, Outcome)` is how an Authorizer answers,
so the order of `Sites()` *is* the interface between the two. A single request is consistent
whatever the order, because its plan is compiled once — but the plan cache is a bounded LRU,
so the same document recompiles after eviction, and an Authorizer that memoized "site 3 is
allow for this operation" would then apply that to whatever site 3 became. Silent,
intermittent, and only under cache pressure. `TestAuthSiteOrderDoesNotDependOnMapOrder`
compiles the document 41 times in fresh executors; without the sort it fails on the first.

**`objectSiteFor`'s memo is what makes a type one question.** Every `__typename` on a guarded
type shares one `SiteObject`. Making it append a fresh site per occurrence also leaves the
suite green — each decision still reaches the right field — but it changes what the Authorizer
is *asked*: the same type once per `__typename` in the document, which a client can repeat for
free, and an Authorizer answering per index can then allow one and deny another for the same
type in one response. `TestEveryTypenameOfOneTypeSharesOneObjectSite` pins one site and checks
that one Allow and one Deny each govern every occurrence.

**Three build-time checks had no test**, each silent in the direction of *less* authorization:
`@public` on an object type exempting its own fields (the blunt instrument of
`RequireAuthCoverage`, and the test also pins that it does not reach types that type refers
to); a `MarkerDirective` naming a directive the SDL never declares, which is what a typo
produces and leaves every field the author believed was marked wide open; and
`combineWithInterfaces`'s `rcapped` branch, where an interface's *own* requirement is already
over the cap so there is nothing valid to AND into the implementer — falling through would
give the implementer a requirement weaker than the interface declares.

**What held up:** the `argSites` contiguity invariant (`TestArgumentSiteDenyRefusesTheField`,
7 subtests), both documented rounds of the `requirementOf` bug (`ForName` dropping all but the
first occurrence; plain `And` paying an unbounded cross product), and the interface-name dedup.
Each fails immediately when reintroduced. One comment was stale: it warned that an inflated
`argSiteCount` would "shift instanceIdx past the instance site", and `instanceIdx` exists
nowhere — the current hazard is that `enforceAuth` reads one site too far and refuses the field
on a deny that was never about it. Corrected in place.

**Not testable, and says so:** `andCapped` widens to `int64` "so a pathological SDL cannot
overflow the check meant to catch it". Reaching that overflow needs a single directive with
billions of groups. The widening is right and cheap; there is no test and there should not be
one.

## And of `exec_object.go`'s guarded-list paths

Same method, different file: 30 uncovered blocks, and the ones that mattered were not the
error branches but the *shapes nobody had written a fixture for*.

**Every guarded-list test used `[Customer!]!`.** With non-null elements a refused row takes
the whole list down, so three branches of `writeListGuarded` had never run — including every
use of `Null()` on a list element. The nullable shape `[Customer]` is the one an author
reaches for precisely so a refused row is a null beside its siblings, and it is the shape
where the outcome actually differs: `Deny` nulls that element and records the index it was
written at, `Null` nulls it silently, and the rest of the list survives both. The fixture in
`authz_instance_nullable_list_test.go` carries both shapes so the contrast is asserted rather
than described.

**The ObjectAuthorizer's failure was tested at `checkObjects` and not past it.** That the
error is redacted and a panic recovered was covered by calling `checkObjects` directly; what
execution then *does* with that error — the largest uncovered region of the file — was not.
Failing open there would hand out every instance-guarded object the moment a policy backend
became unreachable, which is the one failure in this system nobody would notice until an
audit. `TestObjectAuthorizerFailureFailsClosed` drives it at a nullable, a non-null and a list
position, and asserts both that nothing guarded is written and that the backend's address does
not reach the client.

**Which of the two list paths runs is decided by whether the selection happens to contain a
resolver field**, so `TestGuardedListAuthorizesTheSameOnBothPaths` asserts the two against
each other rather than separately: the same policy on the same rows must not authorize
differently because a client asked for one more field. Two things that cost time here and are
worth not rediscovering:

- **The concurrent path also needs `WithMaxConcurrency`.** Without a concurrency budget
  `st.e.sem` is nil, `writeListGuarded` never routes into `writeListConcurrent`, and a test
  meaning to compare the two paths quietly runs the serial one twice — it passes, and proves
  nothing.
- **`sub.Null(); okElem = true` in the concurrent deny is redundant with the splice loop's
  `default: w.Null()`**, so replacing it with `okElem = false` changes no byte of output and
  no test fails. That is not a gap: the two produce the same response, and there is no
  behaviour to assert. A break that *does* fail is one that lets the denied element through,
  which is what the test above catches.

**`RedactRow` is `Redact` decided per row, and it did not get its own `Outcome` field.** A
`Decision` is made before any row exists, so "a customer sees their own email, support sees
everyone's masked" could not be said; `RedactRow(fn(ctx, parent, value))` runs in
`callLeafRedacted` with the row. The first version added a `redactRow` func field to `Outcome`:
48 → 56 bytes, copied into every `Decision` site, and interleaved n=10 measured
`BenchmarkAuthorizerWide64/256` **+17% time, +20-24% B/op** for every authorizer user. It is
now its own `action` (a `uint8`, free) routed through the existing `redact` field, which then
receives a `*redactRowArgs`: memory identical sample for sample, time not distinguishable at
n=14 (p=0.795). The cost -- a closure per `RedactRow` call and an allocation per redacted row --
falls only on its users. It is valid exactly where `Redact` is; accepting it on a composite
field would serve the field unguarded, since the composite path never consults redaction
(`TestRedactRowOnlyOnLeafFields`).
