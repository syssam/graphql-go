---
paths:
  - "exec.go"
  - "exec_*.go"
  - "wave.go"
  - "context.go"
  - "errors.go"
  - "observer.go"
  - "subscription.go"
  - "internal/jsonw/**"
  - "loader/**"
---

# Execution, response limits and scheduling

**Execute (`exec.go`, `exec_object.go`, `internal/jsonw`).** `Executor` owns the plan cache,
the concurrency semaphore, interceptor chains and the limit options. `execState.writeObject`
walks the plan writing into a `jsonw.Writer`; null bubbling rewinds the writer to a recorded
offset rather than building an intermediate value tree. `errNonNull` is the internal signal
for "null reached a non-null position"; `indexedError` carries a list index so error paths
can be reconstructed.

**Three per-request allocations that were there only because nobody looked** (interleaved
n=14, sec/op unchanged on every one of them; allocation counts are the figure):

- `variantKey` returned the `@skip`/`@include` values as a map beside the bitmask, so every
  request built one and a plan cache **hit** dropped it unread. `condValues` is now called
  only where a compile needs it. `TestPlanCacheHitBuildsNoConditionMap` pins a hit at zero
  allocations, because nothing else can see the map.
- `withField` used `context.WithValue`, so a resolver field paid two allocations, one for the
  `FieldContext` and one for the wrapper pointing at it. `fieldValueCtx` embeds it by value:
  `BenchmarkExecuteConcurrentList` 530 → 428 allocs/op. **The new risk is delegation** —
  `Value` must pass every other key through, which `context.WithValue` could not get wrong,
  and breaking it on purpose failed exactly one subscription test.
  `TestResolverContextDelegatesEveryOtherKey` asks on the query path instead.
- `writeList` allocated a `pathNode` per element. A chunked `pathSlab` (4 growing to 128)
  makes a 1000-element list ~12 allocations instead of 1000: `BenchmarkLazySeqSlice`
  3914 → 2926. **The chunk size is the whole trick** — a flat 32 cost a four-element list
  1280 bytes for 160 bytes of nodes, +123% B/op. The invariant is that a node outlives the
  iteration that made it, because a `FieldContext` materializes its path whenever it is
  asked; **errors do not test that**, since `addFieldError` materializes there and then, so a
  slab refilling one chunk in place passed the first version of the test and the whole suite
  with it. `TestListElementPathNodesSurviveTheLoop` reads the `FieldContext` afterwards.
  The trade is disclosed rather than hidden: a three-element list now takes a four-node chunk,
  so `BenchmarkExecuteTypenameHeavy` pays **+4.45% B/op** (2.633Ki → 2.750Ki, +40 bytes a
  list) for -3 allocs/op. B/op is down on every other benchmark and -2.75% geomean.

**The response byte limit (`WithMaxResponseBytes`, 64 MiB by default) is counted on the
writer, not on `execState`.** `execState` has no padding left, and concurrent fields and list
elements write into their own pooled sub-writers that the root writer never sees until they
are spliced, so the budget is embedded in the root `jsonw.Writer` and shared by pointer with
every sub-writer (`ShareLimit`). Each `writeFieldValue` checkpoint reports the writer's growth
since its last report (`OverLimit`) — on every call, not in fixed-size blocks, because a list
element's buffer is usually far smaller than any block and ten thousand of them would never
report. **Splicing must move the count, not copy it**: sub-writers are put back only after
every sibling is spliced, so `Raw` there let a later checkpoint count the same bytes twice and
rejected a response that fit at about half its real size, depending only on field order;
`Splice` hands the sub-writer's reported bytes to the parent. `Put` resets even a buffer it
will not pool, or an oversized sub-writer never gives its bytes back. Once a checkpoint sees
the limit passed the response is rejected even if null bubbling later rewinds those bytes,
because everything after that point was cut short. A trip also makes every failure bubble,
nullable or not (`writeField`, `writeList`, and a check after each concurrent group): before
that, a nullable field or element turned the failure into a null and traversal went on, so a
lazy list of nullable fields was pulled to the end and the root writer reached 229 times the
limit. A concurrent list still drains its source into a slice before spawning, which predates
the limit. **A concurrent list must still spawn every element after a trip**: `pushWave` has
announced them all and a loader flushes only once every announced task has begun, so breaking
out of the spawn loop stranded every parked `Load` and the request hung until its deadline,
holding concurrency slots shared by every request on the executor
(`TestLoaderResponseLimitDoesNotStrandWave`, in `loader/` because the root package's tests
cannot import it). The late tasks fail at their first checkpoint instead. The final size check
is otherwise exact; the in-flight bound is about one field's output per concurrently written
buffer. The errors list is not counted by this limit; `WithMaxErrors` bounds it separately.
The default was set by measurement: interleaved n=12, `BenchmarkResponseLimitOff` against `On` was 15.51µs vs 15.65µs (p=0.347) with 92 allocs/op both, and
`BenchmarkFieldPathBare` against `main` 1.218µs vs 1.281µs (p=0.219) at 18 allocs/op both,
measured with no limit before the default changed —
not distinguishable on this machine, which is a bound on the cost, not a proof it is zero.
The benchmark is one request writing 393 bytes, so neither larger responses nor contention on the
shared counter from a wide concurrent list were measured.
**A CPU profile disagrees with all of this and is wrong.** `jsonw.(*Writer).overLimit` reads as
the largest single line on the hot path — 9.32% flat, and 460ms of 4.83s on the `w.OverLimit()`
line in `writeFieldValue` — because it is a `//go:noinline` leaf and the sampler piles onto the
call. Two measurements say otherwise. Making the whole checkpoint `return false`, which is the
ceiling, bought `BenchmarkFieldPathBare` -3.01% (p=0.018) and nothing significant on
`BenchmarkExecuteUsers` or `BenchmarkExecuteConcurrentList`, interleaved n=12. And giving the
budget an unshared fast path that skips the `LOCK`-prefixed add (a `shared` flag set before any
sub-writer is handed off) bought **zero** on both small benchmarks and cost the concurrent list
+3.07% (p=0.028): the atomic is not what this costs, so there is nothing here to win. Do not
re-propose it on the strength of a profile.

**`maxPooledCap` (16 MiB) is sized against a measured response, never against a type count.**
A buffer whose capacity passes it is reset and dropped, so the next request rebuilds from 512
bytes -- and `append` overshoots, so the cliff sits well below the cap: at 16 MiB a response
stops being pooled at about 15.7 MB. The cap has been wrong twice, both times by extrapolation.
8 MiB was derived from "introspection to roughly 8 000 types" at the 0.79 KB per type a
synthetic schema showed; the real consumer schema is 1.25 KB per type, so its 5 516 types
introspect to 6.56 MB, which reaches 8.05 MiB of capacity and was dropped on every request --
71.3 MB/op flat, against 31.5 once it fits. Introspection response size is a function of type
*width*, so `TestPoolCapCoversALargeSchemasIntrospection` pins the measured 6.56 MB and
`BenchmarkIntrospectOverPool8` reproduces it at 8 400 synthetic types. Raising the cap is
cheap in a way that reads worse than it is: `sync.Pool` is drained by the collector, and a
buffer can only be pooled if a request already paid for it.

**`Selection` must not expose the expansion DAG, and `Fields`'s dedup is what stops it.**
Abstract expansion is memoized, so a plan reaches one `*selectionSet` from several parents,
and every walk inside this package memoizes on it (`authz_shape.walk`, `queryCostMemo`).
`Selection` hands that same plan to an interceptor -- which **cannot** memoize on it, because
`*selectionSet` is unexported and `Selection` is the only handle there is. What keeps a naive
interceptor walk linear is that `Fields` yields one field per response key and `Sub` returns
one selection, so what an author sees is the merged tree rather than the DAG. Dropping the
`seen` map in `Fields` is a one-line change that breaks no other test and takes a 4-deep,
4-wide abstract plan from **7 visits to 424**, growing as width^depth
(`TestSelectionWalkIsLinearInTheMergedTree`). Any new accessor on `Selection` owes the same
property.

**Errors are reported in document order** (`sortErrorsByDocumentOrder`, called once in
`finishResponse`). Fields finish in whatever order their resolvers return, so the same query
used to answer the same errors in a different order run to run — 200 executions of a
six-error query gave **150 distinct orderings** — while `data`, written by walking the plan,
was stable. graphql-js, graphql-http, Apollo Server and graphql-yoga all report in document
order, and build errors here were already sorted for the same reason. The sort key is carried
on `pathNode`: a field's position in its selection set, or a list element's index, one entry
per segment. It rides in padding the struct already had, so `pathNode` is still 40 bytes, and
the ordinal reaches it as an `int32` threaded from the loops in `writeObject` and
`writeFieldsConcurrent` that already have it — `planField` is exactly full at 176 bytes and
could not hold it. Interleaved n=12: allocs/op and B/op identical on `FieldPathBare`,
`ExecuteUsers` and `ExecuteConcurrentList`, sec/op not distinguishable on any of them
(p=0.856, 0.855, 0.235). The sort is stable and an error with no key — the
`ERROR_LIMIT_EXCEEDED` notice — stays last.

**`WithMaxErrors` (1000 by default) bounds the errors list, and checks before it allocates.**
Field errors — `fieldError`, a null in a non-null position, an unresolvable abstract type — go
through `addFieldError`, which returns before materializing the path or calling the presenter
once the list is full, and re-checks under `st.mu` before appending so concurrent fields cannot
overshoot. The first error past the limit becomes one `ERROR_LIMIT_EXCEEDED` notice, which is not
presented, so its code is what `fullLocked` finds it by — `execState` has no room for a flag.
Engine errors that say why a request stopped (cancellation, timeout, an oversized response) go
through `addError` and are never dropped. `requestError` cuts parse and validation errors the
same way. **A dropped field error can be the only record of why a request stopped**: a slow
field often reports a timeout or cancellation through its own error with no later field reaching
the checkpoint, so `droppedFieldError` calls `recordCancellation` when the dropped error is a
context error — the first version lost `OPERATION_TIMEOUT` entirely behind a full list
(`TestMaxErrorsKeepsWhyTheRequestStopped`). Under concurrency a racing field can be presented and
then dropped at the lock, so a logging presenter may run a few more than n times. Not bounded: a leaf list's per-element failures are gathered into one `elementErrors`
before `fieldError` sees them, so a million failing scalars still allocate a million
`indexedError`s first.

**`WithOperationTimeout` is off by default and bounds work, not latency.** It derives a
`context.WithTimeoutCause` in `Execute` (so parsing and every interceptor are inside it) and
per event in `pump` (never around a whole stream). The cause is compared by identity
(`Executor.timedOut`), which is how `OPERATION_TIMEOUT` is told apart from a deadline the
caller set, whose executor error stays `REQUEST_CANCELLED` (a resolver's own error for a
caller deadline is passed through untouched): the split is by who owns the deadline. Both the
`writeFieldValue` checkpoint and `fieldError` translate it: a resolver that honours its context
returns the bare `context.DeadlineExceeded` (or `context.Cause(ctx)`, which `timeoutError.Is`
matches to it), so without the `fieldError` half a single slow field would never report a
timeout at all. **The translation wraps, it does not replace**: the resolver's error stays in
`Err` and an `*Error`'s extensions are copied beside the code, because the first version built a
fresh error and a presenter could no longer find the deadline or the driver's message. Opening a
subscription is deliberately unbounded — interceptors, the Authorizer and the source opener share
the context the stream lives on — and an HTTP batch of n runs n operations in sequence.
`TestOperationTimeoutCoversInterceptors` is the only test that notices the deadline being started
inside `execute` instead of `Execute`. It cannot force a
response out on time, because `taskGroup` waits for every resolver it started; a resolver that
ignores its context still holds the response. Data already written is kept, as with
cancellation. Set, it costs one timer context per request: interleaved n=12 on
`BenchmarkFieldPathBare`'s query, 1.425µs vs 1.881µs (+32%, p=0.000), 18 vs 22 allocs/op,
993 vs 1266 B/op — about half a microsecond a request, large only against a query that small.
Unset it is one nil compare in `Execute`. Its tests run in `testing/synctest`, so every
deadline is exact and instant.

**The pure/resolver split is the core scheduling contract.** `Field`/`FieldArgs` are pure
data access and always run inline with no goroutine and no context allocation: the
`FieldContext` is attached to the context only for resolver fields, and a field interceptor
receives it as its `fc` parameter instead (so `FieldFrom`/`PathFrom`/`SelectionFrom` find
nothing inside a pure field, interceptor or not). A registered interceptor still builds a
`FieldContext` per field; an observer builds none. `Resolve`/`ResolveArgs` may do I/O and are scheduled concurrently, one goroutine each;
the semaphore is a slot budget that `Stats` reports and a task never waits on, because a task waiting for a slot has not begun and its wave could never dispatch (`taskGroup`). `Inline()`/`Concurrent()` override per field. `loader.Loader` (`loader/`)
coalesces `Load` calls within one concurrent wave — the executor announces a wave before
launching sibling tasks (`pushWave`), which is what makes DataLoader batching work.
**A batch's context comes from the Loads waiting on it, never from the first `Load` of the
request** (`batchContext`): values from one waiter, cancelled only once every waiter has given
up, deadline the latest of theirs. The scope used to keep the first `Load`'s context, so a
resolver that wrapped its own in `WithTimeout` and returned failed every later wave with
`context.Canceled` (`TestALaterWaveDoesNotInheritAnEarlierLoadsContext`). **Waiters are
deduplicated by `Done()` channel, not identity**: every resolver's context is its own value-only
wrapper, so an identity check sent every batch down the `AfterFunc`-per-waiter path and cost
~300 allocs per 100-row request (`TestBatchContextSharesOneCancellationWithoutWrapping`,
`BenchmarkLoader*`). Outside `Execute` a
`Loader` caches only under `loader.WithScope`; the unscoped fallback is shared by the whole
process and must not cache (`TestOnlyAScopedLoadIsCachedOutsideExecute`).
**The coordinator counts the whole operation, not a stack of waves.** It keeps three numbers:
announced tasks not yet begun, goroutines running (the operation's own goroutine plus every
begun task, less those parked or waiting in `taskGroup.wait` for tasks of their own), and parked
ones; it dispatches when nothing is pending or running and something is parked. The stack it
replaced counted every park, begin, end and pop against whichever wave was on top, and sibling
subtrees push and pop concurrently, so `pop` removed another subtree's wave and a `Load` waited
for its deadline (`TestLoadIsFlushedWhenWavesPopOutOfOrder`, in `loader/`, under `synctest` for
an exact interleaving). It also needed a tick fallback for a "spent" wave, the state an
`Inline()` resolver's `Load` parks into after `g.wait()`; with the launcher counted as running
again after its wait, that `Load` dispatches at once (`TestLoadFromAnInlineResolverIsFlushed`,
`TestWaveCoordinatorDispatchesALoneLoad`) and there is no fallback left to arm too early.
**The launcher holds back dispatch until it waits** (`TestWaveCoordinatorWaitsForTheLauncher`):
it can still start work that queues keys. Interleaved n=12 against the stack:
`BenchmarkExecuteConcurrentList` -11.06% (p=0.001), -2 allocs/op; -16 B/op on every request
because the coordinator is 48 bytes; `FieldPathBare` and `ExecuteUsers` not distinguishable.

**Subscriptions (`subscription.go`).** `Subscribe`/`SubscribeArgs` bind a subscription root
field to a function returning `<-chan R`; `Executor.Subscribe` plans the operation, opens
the stream and returns `<-chan *Response`, one per event, closed when the source closes or
ctx is cancelled. Each event builds its own `OperationContext` and runs the whole operation
chain, so a DataLoader cache cannot outlive the event that filled it. The per-event writer
substitutes the root field's executor with one that yields the event and then calls the
ordinary `writeObject`, which is why null bubbling and error paths need no special case —
and why field interceptors do not see that one field. A `FieldObserver` runs before that
substitution in `callLeaf`/`callResolve`, so it does see the root field, once per event. A subscription root field bound with
`Field` or `Resolve` is rejected at `NewSchema`.

**Adding a field to `execState` or `OperationContext` is a hot-path change.** Both are
allocated per request and both sit exactly on a size-class boundary. An `atomic.Int64`
counter on `execState` measured +3.2% B/op with the feature disabled; as an `atomic.Int32`
packed beside `cancelled` it measures zero. Check with `unsafe.Sizeof` and `benchstat`
before growing either, and interleave the runs — a non-interleaved comparison on this
machine reported a 13.8% regression that vanished at n=18. **`OperationContext` holds
its `WaveCoordinator` by value**, which pays for itself only while the two land on one size
class: embedding the 64-byte stack coordinator made 160 + 64 = 224 exactly, -1 alloc/op at
B/op unchanged (interleaved n=14, `BenchmarkExecuteUsers` 15 -> 14 allocs/op). The counting
coordinator is 48 bytes, so it is 208 now, itself a size class. One more word crosses to 224
and costs 16 bytes a request, which is why `TestStructSizes` asserts 208 rather than logging it. The
`hub *WaveCoordinator` pointer stays beside the value because `Waves()` documents returning
nil for an `OperationContext` nobody's executor built, and `go vet`'s copylocks is what
proves nothing copies the struct now that it contains a mutex. **`-count=N` does not
interleave**: `go test -bench` runs all N counts of one benchmark before the next. Build the
test binary once (`go test -c`) and alternate separate invocations, or alternate two
binaries. Allocation counts are deterministic, so batched runs are fine for those.

`internal/jsonw` is the output writer and has no dependency on engine types — the plan
compiler and executor deliberately live in the root package so generic constructors can
produce engine values directly.

**A nil slice at a non-null list is `[]`, not a null violation.** It was the violation until
2026-09-25, and the CLAUDE.md convention told resolvers to return `make([]T, 0, n)`. Measured,
same schema and input: graphql-js 17.0.2 errors on `resolve: () => null` for `[Int!]!` and
writes `[]` for `[]`, which decides nothing because JS has no nil slice; gqlgen v0.17.95 writes
`[]` for a nil slice (its template returns `graphql.Null` for a nil slice only when the list is
nullable). So every gqlgen resolver returning nil for an empty non-null list -- ORMs return nil
for no rows -- failed after migrating, at request time, where neither the compiler nor
`NewSchema` could see it. `valueShape.nilIsEmpty` is decided in `shapeFor` (a Go slice at a
non-null list), so the executor tests a bool on the nil path and reflects on nothing;
`listWriter` does the same for leaf lists. A nullable list keeps nil as `null`, so nil and
`[]T{}` still differ there. The graphql-js differential records `whole list null, [String]!` as
an intended divergence, asserted exactly: a Go slice has no null to give at that position, and
refusing a list is what an error is for. `TestNilSliceAtNonNullListIsEmpty` and
`TestLeafWriterShapes` each fail with their path's check removed.
