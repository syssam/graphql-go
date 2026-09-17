# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go vet ./... && go test -race -count=1 ./...   # the standard gate for every change
go test -race -run TestExecNestedLists .       # one test in the root package
go test -short ./codegen                       # skip the slow subprocess compile tests
go test -run xxx -bench . -benchmem .          # microbenchmarks
go test -run xxx -fuzz FuzzExecute -fuzztime 30s .
```

**`go test ./...` reaches one module and this repository has four.** `benchmarks/`,
`compare/` and `lint/` are outside the root module, so the gate above does not test them:

```sh
sh scripts/gate.sh            # vet and test every module
sh scripts/gate.sh -short     # skip the slow subprocess and load tests
```

Use it before claiming a change is clean. During one audit the three outside modules were
very nearly missed, and they had a root API change in them at the time.

**Watch for measurements that succeed while covering less than they look.** A red build is
easy; a command that used to be complete, quietly stopped being, and still prints success is
not. Four instances in one session, all found by asking what a passing result would look
like if the thing under test were broken:

- `go test ./...` printed `ok` for every package it knew about, and had silently stopped
  reaching three modules as they were added.
- A subscription leak test passed against a deliberately broken release path, because a
  second redundant path still freed everything. It only failed when both were broken.
- A fan-out benchmark reported 45ns per subscriber because it timed a `publish` that drops
  into a full buffer. Counting receipts put it at 4us — the events it was "measuring" had
  reached nobody.
- Generated code that read correctly and did not compile, twice: a method taking the
  generated args struct (import cycle) and `ID!` bound to a `string` field. Both were found
  by compiling the output, neither by reading it.

The habit that catches these is breaking the thing on purpose and requiring the test to
fail. If it still passes, the test was agreeing with the code rather than checking it.

`benchmarks/` is a separate module (with a `replace` back to the root) because it
depends on gqlgen:

```sh
cd benchmarks && go test -run '^$' -bench . -benchmem -count=5
cd benchmarks && go generate    # regenerate graph/generated.go via go tool gqlgen
```

Regenerate the example's bindings after editing its SDL:

```sh
cd examples/blog && go generate    # go tool gqlc -config gqlc.yaml
```

`codegen` tests write a temp module, run `go build`/`go test` in it, and take ~20s; they
honour `-short`.

**`-race` is not optional.** The DataLoader N+1 race was caught 4 times in 40 runs with
`-race` and 0 times in 40 without it: the detector perturbs scheduling enough to hit the
interleaving. Dropping `-race` to save time silently disables the only thing that finds
this bug class. `testing/synctest` does not help here and makes it worse — its
deterministic scheduling hid the same bug in 20 of 20 runs. Use synctest for tests that
would otherwise wait on real time (see `wave_test.go`), not to find races.

Comparing performance between two versions goes through `benchstat`, not by eye:

```sh
go test -count=10 -run '^$' -bench . -benchmem > old.txt   # before
go test -count=10 -run '^$' -bench . -benchmem > new.txt   # after
benchstat old.txt new.txt                                  # golang.org/x/perf/cmd/benchstat
```

Single samples on this codebase have been wrong by 20-77% when the machine was warm.

**Two sequential runs measure the machine as much as the change.** Contention that
arrives between `old.txt` and `new.txt` lands entirely on one side. On a busy machine —
several agent sessions and their language servers — build two test binaries and alternate
them, so every unit of contention is shared:

```sh
git worktree add --detach /tmp/base HEAD && cd /tmp/base   # then edit back to "before"
go test -c -o /tmp/before.exe .                            # and from the main tree:
go test -c -o /tmp/after.exe .
for i in $(seq 1 12); do
  /tmp/before.exe -test.run xxx -test.bench . -test.benchmem -test.count=1 >> before.txt
  /tmp/after.exe  -test.run xxx -test.bench . -test.benchmem -test.count=1 >> after.txt
done
benchstat before.txt after.txt
```

This is not the same as more runs. A sequential comparison of a change that does strictly
less work reported it 50% *slower* here; interleaved, the same change came out
-3.76% (p=0.040, n=12) with allocations equal sample for sample, while per-sample spread
stayed at +/-25% because the machine really was that noisy. The spread survives; the
comparison does not have to.

## Architecture

Schema-first runtime with a code-first binding API. SDL is the contract; Go bindings are
plain function values. Everything expensive happens once, at start-up or first plan
compile — the request path writes JSON straight into a pooled buffer.

**Build time — `NewSchema` (`schema.go`, `source.go`, `registry.go`, and the binding files
`scalar.go`, `enum.go`, `object.go`, `abstract.go`, `input.go`, `directive.go`).**
`gqlparser.LoadSchema` parses the SDL, then `SchemaOption`s (`Object`, `Field`, `Resolve`,
`Input`, `Args`, `Enum`, `Scalar`, `Interface`, `Union`, `Directive`, `Query`/`Mutation`/
`Subscription`) register typed adapters in `registry`, keyed by `(GraphQL type name,
reflect.Type)`: leaf writers, decoders, nil checks, list traversers, `shapeInfo`.
`schemaBuilder.build()` then runs six ordered phases — object shells → input decoders →
abstract types → fields → schema directives → coverage validation. Order matters: shells
must exist before fields reference them, inputs before args are composed. Go-vs-SDL shape
mismatches are reported here as joined errors, never at request time.

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
error rather than a nil entry if the parse panicked. Known and accepted: `docMu` is one lock for
the executor, and a miss hashes the query text under it, so misses for very large different
texts queue briefly behind each other. The tests hold the parse open with `testHookParseDocument` inside `testing/synctest`,
so "concurrent" is exact. Each `(operation, @skip/@include variant)` compiles to an
immutable `plan` with fragments flattened, directives constant-folded per variant (up to
`maxCondVars` boolean variables), arguments pre-decoded and response keys pre-serialized.
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
an error `next` on every event as it did before this guard existed.

**Execute (`exec.go`, `exec_object.go`, `internal/jsonw`).** `Executor` owns the plan cache,
the concurrency semaphore, interceptor chains and the limit options. `execState.writeObject`
walks the plan writing into a `jsonw.Writer`; null bubbling rewinds the writer to a recorded
offset rather than building an intermediate value tree. `errNonNull` is the internal signal
for "null reached a non-null position"; `indexedError` carries a list index so error paths
can be reconstructed.

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
`FieldContext` per field; an observer builds none. `Resolve`/`ResolveArgs` may do I/O and are scheduled concurrently under
the bounded semaphore; `Inline()`/`Concurrent()` override per field. `loader.Loader` (`loader/`)
coalesces `Load` calls within one concurrent wave — the executor announces a wave before
launching sibling tasks (`pushWave`), which is what makes DataLoader batching work.

`transport/gqlws/load_test.go` opens 150 concurrent subscriptions and requires both the
source registrations and the goroutines back afterwards; it honours `-short`.
`BenchmarkSubscriptionFanout` measures a broadcast reaching every subscriber by counting
receipts, because `publish` drops rather than blocks and timing it alone reports a fan-out
to 128 clients at 45ns each when the honest figure is 4us. **Releasing an operation is
doubly redundant** — the operation context derives from the connection context, and
`cancelAll` also calls each stored cancel — so breaking either leaves every test green and
only breaking both leaks. Do not read a green suite as evidence that one of them is dead.

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

**Codegen (`codegen/`, `cmd/gqlc`).** SDL-only: it never loads Go packages. It emits models,
args structs, a `Resolver` interface and bindings that call the same public constructors as
hand-written code. Each group emits a single `generated.go` holding its args, `Resolver`
interface and bindings — one file per package, since that is the unit the compiler rebuilds.
`Config.Manifest` binds types and fields outright instead of inferring them, which is the
mode an external generator wants; it still loads no Go type information, so a method binding
declares its own shape (`Context`, `Error`). Type bindings are folded into `cfg.Models` in
`newBuilder`, so model references, imports and `mapped` keep working unchanged and only
field kinds and group overrides are read from the manifest afterwards. **A method's
arguments are spread into the call, not passed as the generated args struct** — passing the
struct makes the bound type's package import the generated one, which already imports it for
the model, and that is an import cycle. A type in the manifest gets no inference at all: an
unlisted field is a resolver.

`Config.AutoBind` discovers bindings from named packages instead of being told them, and
produces a `Manifest` — so discovery is the only new behaviour and everything downstream is
the manifest path. **Only export data is loaded** (`NeedName | NeedTypes | NeedImports |
NeedDeps`): adding `NeedSyntax` or `NeedTypesInfo` would parse every file of every package,
which is the cost this project exists to avoid, so treat either as a regression. Matching is
json tag, then case-insensitive name, then a method whose shape the generator can call;
anything else falls through to `Resolver`, because a resolver method can always be written
where a bad guess is a compile error in code the user did not write. A Go type over the same
basic kind gets a conversion (`graphql.ID(v.ID)`), since an ORM storing an id as a `string`
still answers `ID!` and refusing would send the commonest field in any schema through a
resolver.

One SDL group stays flat in `Output`; two or more become subpackages plus a `Resolvers` struct, with models split the same way (`model/<group>/`) so a one-group edit
does not invalidate every other group's compiled package — except when two groups' input
objects reference each other, which would be an import cycle and falls back to one shared
`model` package (`modelGroupsAcyclic`). Generated files are strings run through `go/format` (not Jennifer), and
content-equal files are not rewritten.

**Transports.** `transport/gqlhttp` is the GraphQL-over-HTTP handler; `transport/gqlsse`
streams over Server-Sent Events (distinct connections mode); `transport/gqlws` speaks
`graphql-transport-ws` over `coder/websocket`; `transport/gqlecho` and `transport/gqlfiber`
add Echo v5 and Fiber v3. All five serve every operation kind — a query or mutation is one
`next` then `complete` — so a client needs one endpoint. `internal/httpreq` expresses its
CSRF, body-limit and decoding rules over a `Source` accessor, and owns `Negotiate` (Accept
q-values, and with them the response `Content-Type` and whether a request error is 200 or
400), so `gqlhttp`, `gqlsse`, `gqlecho` and `gqlfiber` share one rule set and a request one
rejects as forgeable or oversized is rejected by all of them;
`transport/equivalence_test.go` proves this by driving real requests through all six
HTTP-carrying handlers — both SSE handlers included, since `gqlfiber`'s is hand-written —
rather than by inspecting the code. The one documented exception: "mutations are not allowed
over GET" is composed independently per transport, not through `httpreq`, and the SSE family
(`gqlsse`, `gqlecho.SSE`, `gqlfiber.SSE`) and the plain-HTTP family use different wording for
it, as they do for the unacceptable-`Accept` message — a real split along transport kind, not
drift to fix. A row that splits still asserts every handler on both sides of it.

`gqlecho` is `net/http` underneath, so it delegates to `gqlhttp`/`gqlsse`/`gqlws` rather than
reimplementing them; its only addition over `echo.WrapHandler` is mapping a pre-response
failure (rejected method, unacceptable `Accept`, forgeable request) into an `*echo.HTTPError`
so Echo's error handler and middleware see it. `gqlfiber` is fasthttp-native instead: parsing
goes through `httpreq`'s fasthttp `Source`, writing goes straight into the fasthttp response
buffer via `Response.WriteTo`, and no `net/http` value exists anywhere on the path. Fiber's
own `Ctx` can never be cancelled — `Done()` is always nil, and `Context()` is
`context.Background()` unless middleware set one — so every `gqlfiber` handler derives its
own cancellable context; without it, `Executor.Subscribe`'s teardown has nothing to unwind
through. **That derived context cancels on handler return, not on client disconnect**: fine
for unary requests, but the SSE handler parks inside `SendStreamWriter` for the whole stream,
so a failed `w.Flush()` — fasthttp's only disconnect signal — is what drives the cancel.
Getting this backwards produces a leak test that cannot fail, which this branch's own plan
did once, caught in review before it reached a commit. Two related, deliberate limits: `gqlfiber`'s WebSocket sets no read deadline (the
only candidate interval is `PingInterval`, and the protocol tracks no pongs, so a derived
deadline would drop slow-but-live clients), and `WithKeepAlive(0)` on its SSE leaves an idle
subscription with no write that can fail, so it is held open until its source ends (or `WithMaxStreamAge` ends it) — the
handler warns at construction rather than reinterpreting the option's meaning. Its WebSocket
layer (`gofiber/contrib/v3/websocket`, over `fasthttp/websocket`) has no origin-check hook of
its own, so `transport/gqlfiber/ws.go` hand-rolls one mirroring `coder/websocket`'s semantics
branch for branch — keep it a mirror; divergence there is a security divergence.

**Long-lived connections are drained by `transport/drain`, not by the servers.** `net/http`'s
`Shutdown` says so in as many words ("does not attempt to close nor wait for hijacked
connections such as WebSockets"), and an SSE stream is an active request that never ends, so
`Shutdown` waited out its whole deadline and the process then cut the stream. One `drain.Drain`
is handed to `gqlws`/`gqlsse`/`gqlfiber` through `WithDrain` (a shared object rather than a
method, because `gqlfiber` and `gqlecho` return handler functions) and shut down *alongside* the
server, never before or after: `srv.Shutdown` waits for SSE handlers that only the drain ends.
On `Closing`, a WebSocket (`internal/gqlwsproto`, both drivers) refuses new operations with an
`error`, cancels subscriptions without `complete`, lets queries and mutations finish so their
clients learn the result, and closes 1001; an SSE subscription stream returns without a
`complete` event. Omitting `complete` is the point — it would tell graphql-ws and graphql-sse
clients the subscription ended for good instead of reconnecting. New WebSocket connections and
new SSE subscriptions get 503. Past its deadline `Shutdown` cancels every entered context and returns without waiting; the wait
goroutine outlives that return until handlers call `leave`. **`gqlwsproto.watch` closes the
socket itself** when its parent context is cancelled, including mid-drain while an operation
ignores cancellation: `gqlfiber`'s `Read` ignores its context, so closing the socket is the only
thing that ends it, and the first version of the drain waited on the operations with nothing
watching the parent and never closed at all. **`gqlwsproto` reads under
`context.WithoutCancel`**: coder/websocket closes a connection with no frame the moment a read's
context is cancelled, so on `gqlws` a drain giving up produced EOF instead of 1001 — caught only by
a transport-level test, since the protocol's fake socket cannot close anything. A read now ends
only when the socket closes, so every path that ends a connection must close it. **A cancelled
connection context — the one `OnConnect` returned, say on token expiry — closes the socket with
1001 through a `context.AfterFunc`** registered after the ack and unregistered first in `serve`'s
defer (so Serve's own cancel on a normal exit sends nothing); both drivers need it now that
neither read is cancellable, and `gqlfiber` never had it. `Serve` releases `watch` from a defer,
so a panicking `OnConnect` does not leave it parked. Three drain divergences are deliberate and
known: `gqlws` checks the drain before anything else, so a non-upgrade GET while draining gets
503 where `gqlfiber` answers 426, then 403 (origin), then 503; drain refusals are not in
`transport/equivalence_test.go`; and `gqlecho.serve` does not map a drain 503 to an
`*echo.HTTPError`. Two guards in `gqlwsproto` have no deterministic
test and say so beside them (`closed(cfg.Closing)` in `subscribe`, and `Serve` waiting for
`watch`); a reviewer's 50-run break of each failed 0 and 3 times. `gqlhttp` needs nothing.

**Connection age and idle limits mirror grpc's `keepalive.ServerParameters`**, all off by
default. On a WebSocket (`WithMaxConnectionAge(age, grace)`, `WithMaxConnectionIdle` on `gqlws`
and `gqlfiber`, both driving `gqlwsproto.Config`) age is counted from `Serve` starting and spread
±10% by `internal/jitter.Spread`, so connections opened together do not reconnect together; it
runs the shutdown drain for that one connection (refusal message "The connection has reached its
maximum age.", subscriptions end without `complete`, queries finish, 1001), and a positive grace
closes 1001 anyway once it passes. `Spread` clamps at `MaxInt64`: the first version wrapped
negative for huge ages, so setting an age of "never" drained every connection at once. Idle means
no operation in flight — subscriptions count, pings and a `complete` for an unknown id do not — and
closes 1000, since nothing was lost. A rotation is not free for a busy client: an operation sent
while the age drain waits for a long query gets a terminal `error`, which graphql-ws does not
retry, and that happens every age period, not only at shutdown. **The idle timer's `Stop` in `subscribe` and `closeIfIdle`'s
re-check under `mu` are deliberately redundant**: breaking either alone leaves every test green,
and only breaking both fails, the same shape as `cancelAll`. `closeIfIdle` also refuses to close
before `idleDeadline`, for a timer that fired just as a fast query started and finished; no test
forces that, and none covers `serve`'s defer stopping the timer. SSE has only
`WithMaxStreamAge` (`gqlsse`, `gqlfiber`): a stream is one subscription, never idle while open,
and ends without `complete` so the client reconnects. The protocol-level tests run in
`testing/synctest`, hours of fake time exact and instant; the transport tests use real 100 ms
limits with deadlines, since sockets cannot run in a bubble.

`internal/gqlwsproto` is `graphql-transport-ws` extracted so `gqlws` and `gqlfiber`'s
WebSocket layer both drive it. It locks around every write: `coder/websocket` serializes
writers itself, but `fasthttp/websocket` (a gorilla derivative) does not, and concurrent
subscriptions on one connection all write to the same socket. `Close` is deliberately outside
that lock and serializes itself instead — an interleaved close frame corrupts the stream.
Subscription release is double-secured on purpose: operation contexts derive from the
connection context (so `cancel()` alone frees every one) and `cancelAll` also cancels each
subscription explicitly. Either path alone suffices, so **a green test suite is not evidence
that either one is dead code** — breaking each half separately still passes every test; only
breaking both leaks. See the comment on `cancelAll` in `internal/gqlwsproto/conn.go`.

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

**Adding a field to `execState` or `OperationContext` is a hot-path change.** Both are
allocated per request and both sit exactly on a size-class boundary. An `atomic.Int64`
counter on `execState` measured +3.2% B/op with the feature disabled; as an `atomic.Int32`
packed beside `cancelled` it measures zero. Check with `unsafe.Sizeof` and `benchstat`
before growing either, and interleave the runs — a non-interleaved comparison on this
machine reported a 13.8% regression that vanished at n=18. **`-count=N` does not
interleave**: `go test -bench` runs all N counts of one benchmark before the next. Build the
test binary once (`go test -c`) and alternate separate invocations, or alternate two
binaries. Allocation counts are deterministic, so batched runs are fine for those.

`QueryCost.Connections` prices a Relay connection by its `first`/`last`, which sit on the
connection field one level above the `edges` list they bound and are otherwise invisible:
without it `conn(first: 2)` and `conn(first: 200)` both cost `DefaultListSize`. The page size
pays for the list directly inside it and nothing deeper, or the two multiply and a page of
200 prices as one of 2000. Off by default: it changes the number an existing deployment set
`Max` against. The walk carries its state on `costWalk`, and **a memo over it must key on
`paid` as well as the selection set** — the same `*selectionSet` costs differently paid and
unpaid.

`ext/otel` instruments an executor with OpenTelemetry: `graphql.NewExecutor(s, otel.New()...)`.
One span per request, started before parsing so a parse failure still produces one and
renamed once the operation is known. **Metrics are recorded at the operation layer and at
the request layer only when the operation chain never ran** — recording at both double-counts
every request, which the metric test caught. Field spans (`WithFieldSpans`) are opt-in and
are wired through a `FieldObserver`, not a `FieldInterceptor`. `BenchmarkFieldPathBare`,
`BenchmarkFieldPathInterceptor` and `BenchmarkFieldPathObserver` (root package) measure 18,
74 and 18 allocs/op (993, 3900 and 993 B/op) over the same query — but the observer benchmark
registers a no-op observer, so what it measures is the engine's observer call sites costing
no allocations when the observer itself does nothing. They are not free in time: two
interface calls and a defer per field, which a reviewer's alternated runs on a loaded machine
put at roughly +16% over bare — small, not zero, and not a settled figure. The interceptor's
74 allocs/op is the cost of the type-erased write path forced on every field so the
interceptor is able to replace a result, whether or not it uses that ability; an observer
cannot change a result, so the engine never routes a field through that path for one, and a
pure field keeps its typed writer regardless of what the observer does. What `ext/otel`'s own
`fieldSpanObserver` then spends per field it actually observes — a `tracer.Start`, one
`SetAttributes` call carrying two attributes and a `span.End()`, plus `RecordError` and
`SetStatus` when the field errors, on every field including pure ones once `WithFieldSpans`
is on — is real cost that these benchmarks do not measure and this file does not have a
number for. One behaviour changed with the switch: the observer's `EndField` runs
after panic recovery sets the field error, so with recovery on (`WithRecover`, the default) a
panicking field's span now gets error status
— the interceptor's plain `defer span.End()` ran during the panic's unwind, before recovery
converted it, so a crashed field used to produce a span that looked clean.

**Runtime metrics follow the OpenTelemetry runtime instrumentation, not grpc's channelz**: no
introspection endpoint, just asynchronous instruments read on collection. `Executor.Stats()` is the
engine's snapshot (plan cache entries, bytes, hits, misses; concurrency slots in use and limit) and
`drain.Drain.Active()` the transport's (long-lived connections on handlers with `WithDrain`);
`otel.ObserveExecutor` and `otel.ObserveDrain` register them after the executor or drain exists,
returning a `metric.Registration` to unregister, and `otel.New`'s request interceptor adds
`graphql.server.active_requests`. **Hits and misses are counted after `planFor`, not in
`document`**: every streaming transport calls `OperationKind` before `Execute`, and counting in
`document` would count each such request twice (`TestStatsIgnoresRejectedAndKindLookups`); a
request rejected by parsing, validation or a depth or complexity limit is neither, one refused by
the cost limit has been planned and counts, and a subscription is one lookup at `Subscribe` though
its per-event spans repeat `cache_hit`. The counters
are two `atomic.Int64`s on `Executor`, not per-request state: interleaved n=12 on
`BenchmarkFieldPathBare` against `main`, 1.698us vs 1.652us (p=0.311) at 18 allocs/op both —
and a reviewer's `RunParallel` at `-cpu 20` put the shared counters at 671.2ns vs 688.2ns
(p=0.102, not significant), allocations equal. Names follow the repository's split:
`graphql.server.*` where the concept is generic, `graphqlgo.*` where it is this engine's. Usage and
limits are observable UpDownCounters, not gauges, as the OpenTelemetry runtime and connection-pool
conventions have them, so backends can add them across instances; hits and misses are one
`graphqlgo.plan_cache.lookups` counter split by `graphqlgo.plan_cache.result`. **The instruments
carry no identity of their own**: two executors, or one observed twice, on one meter add their
counters and overwrite the rest, silently — the first version shipped that until review measured
it — hence `otel.WithAttributes` and "observe each once per meter" in the godoc. No per-operation
dimension, to keep cardinality bounded. `graphql.server.active_requests` wraps `reqChain`, which
only `Execute` runs, so subscriptions never show there; `graphqlgo.transport.active_connections` is
where they do.

`otel.Batch` / `otel.MappedBatch` wrap a `loader.BatchFunc` so every flush gets a span under
the request that caused it. They live in `ext/otel` rather than as a loader option so
`loader/` keeps depending on nothing but the engine and the standard library.
`graphqlgo.loader.keys` is the attribute to alert on: one key per span means batching has
degraded to N+1.

`ext/apq` is automatic persisted queries, opt-in through `WithPersistedQueries` on either
HTTP transport. **Resolution happens during parsing, not at execution**: a request carrying
only a hash has no query text, so the "mutations are not allowed over GET" guard would have
nothing to inspect and would wave a persisted mutation through. Registration verifies
`sha256(query) == hash` — storing whatever text arrived would let one client choose what
every later client's hash executes. `httpreq` takes a `queryOptional` flag so that with APQ
off the missing-query errors are byte-identical to before.

`ext/trusted` is the same wiring as a safelist: a `Store` is an `apq.Cache`, and `apq.Resolve`
tells the two apart by the `apq.TrustedStore` marker. A safelist must refuse query text
however it hashes — verifying the hash only proves the client can hash — and must refuse a
freeform request carrying no hash at all, which is otherwise the way straight round it.

`ext/throttle` spends what `QueryCost` computes: a bucket of points per caller, refilled on
read rather than on a timer, quoted before the query runs and refunded down to the actual
cost after. **It charges per subscription event**, because each event runs the whole
operation chain; a stream billed once at open is unmetered.

In `gqlwsproto`, **writes use the connection context, never the operation's**: coder/websocket
tears down the whole connection when a write context is cancelled mid-frame, so writing a
`next` under the operation context would let one client's unsubscribe drop every other
subscription on that connection. The init timeout likewise closes the connection from a
timer rather than bounding the read, because a read aborted by its own context leaves no
way to send the 4408 close frame.

`fed/` makes a schema an Apollo Federation subgraph and needs no engine change: `_entities`
is an ordinary root field returning a union, and a union resolves its concrete type from the
dynamic Go type, the same path `Query.node` takes. The prelude adds only protocol
scaffolding no author writes in any implementation, and `_service` returns the author's text
verbatim because that text is what the router composes from. **Only object types with `@key`
are `_Entity` members** — a union's members must be objects, and an interface carrying `@key`
is reached through its implementing types, so a resolver for one is rejected at build time.

`relay/` binds the Relay contract: global ids (`base64("Type:id")`, byte for byte what
graphql-relay-js and graphql-java produce), `Node`, and cursor connections. It emits no
types — the SDL still declares `Node`, the connection and the edge, as in every reference
implementation. `Query.node` needed no engine change: it binds through the existing
`any`-returning resolver on an unbound interface.

`internal/jsonw` is the output writer and has no dependency on engine types — the plan
compiler and executor deliberately live in the root package so generic constructors can
produce engine values directly.

**Authorization is compiled, not wrapped.** `AuthShape` is built once at plan compile
(`buildAuthShape`) and cached with the plan; it does not depend on the principal, so
`planKey` is unaffected and the plan cache is not multiplied by policy. A field that
declares `@requiresScopes` gets `planField.authIdx >= 0` at construction; every other field
gets `-1`, so the ordinary request path pays one integer compare and no allocation.
`execState` (64 bytes) and `OperationContext` (160 bytes) held those sizes through this
branch (`TestStructSizes`, `authz_bench_test.go`) — re-measure both, interleaved, before
adding a field to either; see the `execState`/`OperationContext` entry above for why a
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

Two facts only exist because authorization and bounded plan expansion landed together.
compileSelection's memo makes one `*selectionSet` reachable from several parents, so the
`seen` set in `shapeBuilder.walk` is load-bearing — it keeps the walk linear in the DAG —
and sharing does not conflate decisions, because a site's requirement is read off the field
definition alone. And a Redact outcome runs the resolver, so `callLeafRedacted` carries its
own copy of `callLeaf`'s `FieldObserver` handling, defer order included; Deny, Null and Zero
never resolve and are never observed. Drop that copy and a redacted field is invisible to
`ext/otel`'s field spans while every other test stays green — `TestOutcomeRedactIsObserved`
is the one that notices.

## Conventions

- **Root package may depend only on `gqlparser/v2` and the standard library.** Transports,
  codegen and extensions keep their dependencies in sub-packages. `go.mod` therefore also
  carries `yaml.v3` (for `cmd/gqlc`), `coder/websocket` (for `transport/gqlws`),
  `labstack/echo/v5` (for `transport/gqlecho`), `gofiber/fiber/v3` and
  `gofiber/contrib/v3/websocket` (for `transport/gqlfiber`), and the OpenTelemetry API and SDK
  (for `ext/otel`, the SDK only in its tests); the rule is about what the root package
  imports, not about module purity. **Mind the Fiber websocket module path**:
  `gofiber/contrib/websocket` (no `/v3/`) is the Fiber v2 module and will not build against
  v3 — the path above, with `/v3/`, is the one this repository needs. `ext/otel`, `gqlecho`
  and `gqlfiber` are the obvious candidates to split into their own modules at publication
  time, so those dependencies leave every consumer's module graph.
- **No reflection on the request hot path.** Reflection is allowed at `NewSchema`, in
  `Args[T]`/`Input[T]` decode, and in the one documented composite nested-list traverser
  (which logs `slog.Warn` at start-up). Adding reflection to the write path is a regression.
- **Generated code is driven through `tool` directives, not a `tools.go` blank-import file.**
  `go tool gqlc` (root) and `go tool gqlgen` (`benchmarks`) pin the CLI and its
  dependencies in `go.mod`, so a generate step cannot silently resolve a different version
  than the one the module records. The two `tools.go` files this replaced were redundant:
  every package they pinned was already reached by a real import.
- Go 1.27 minimum (generic methods, `reflect.TypeFor`).
- Tests live beside the code in `package graphql`; the shared fixture schema and executor
  are in `fixture_test.go` (`newFixtureExecutor`, `run`, `expectData`) — reuse them instead
  of building new schemas per test. `exec_conformance_test.go` is the spec-behaviour suite;
  `api_align_test.go` guards the public binding surface.
- Request-scoped extension state goes in via `OperationContext.GetOrSet`, never
  `Get` then `Set` — `lint/` has an analyzer that catches this at compile time
  (`cd lint && go build -o gqlvet ./cmd/gqlvet`, then `gqlvet ./...` from the repo
  root). It finds the original bug instantly where the test found it 4 times in 40
  runs, and only under `-race` — concurrent sibling resolvers hit their first `Load` together, and
  the check-then-act pair silently gives each one its own copy. `-race` will not catch
  it; the symptom is DataLoader batching intermittently degrading to N+1.
- A nil Go slice is written as `null`, so a list field bound to `[T!]!` must return
  `make([]T, 0, n)` rather than a nil slice for an empty result.
- Undo a deliberate break with a reverse edit, not `git checkout -- <file>`: on a file whose
  real change is not yet committed, that reverts to HEAD and destroys the work being tested.
- Package documentation lives in `doc.go`; `graphql.go` holds only the primitive public
  types (`ID`, `Root`, `Omittable`). Moving code between files in a package is free and
  invisible to callers, so keep each file focused enough to guess from its name.
- Comments explain why, not what. English only. No code-narrating comments.
- Commit messages: imperative, lower-case type prefix (`feat:`, `fix:`, `test:`, `refactor:`,
  `docs:`).

## Design documents

`docs/superpowers/specs/2026-09-11-graphql-go-design.md` is the design rationale, but its
**"Phase N Deviations" sections are the authoritative behaviour** where they disagree with
the body (e.g. `Object[User]` not `Object[*User]`; nullable input positions require a
pointer/slice even with a default; complexity/depth/cost are `Executor` options, not an
`ext/complexity` package; `Manifest` and `AutoBind` landed in phase 4 rather than phase 2,
and the phase 4 deviations describe how). Read the deviations before trusting the prose.
`docs/superpowers/plans/` holds the phase implementation plans; `docs/module-layout.md` the
decision to stay one module until publication; and `docs/benchmarks.md` the gqlgen comparison
and the transport cost comparison (`net/http`, Echo, Fiber-native, Fiber-via-`adaptor`). The
latter is also where the strongest argument for `gqlfiber`'s native path lives, and it has
nothing to do with allocations: `fasthttpadaptor` hands the wrapped handler a
`*fasthttp.RequestCtx` as its request context, and `RequestCtx.Done()` is documented as the
**server's** shutdown channel, not a per-request one — so a `gqlhttp` handler reached through
`adaptor.HTTPHandler` never sees a client disconnect, and every in-flight adapted request
sees `ctx.Err() != nil` the moment shutdown begins. `benchmarks/k6/` load tests the HTTP
transports against `benchmarks/cmd/transportserver`; read its README before quoting a number
from it, because the timings it first recorded were single samples and have been retracted.

Status: phases 1-4 complete and merged to `main` — engine, both codegen binding modes,
subscriptions, five transports, DataLoader, APQ, limits with actual cost accounting,
OpenTelemetry with field observation, bounded plan expansion, a plan cache bounded by
query text, a response size limit, an operation timeout, a shutdown drain and connection age and idle limits for WebSocket and SSE, runtime metrics (`Executor.Stats`, `otel.ObserveExecutor`, `otel.ObserveDrain`), the authorization spine (`Authorizer`, `AuthShape`, `RequireAuthCoverage`,
`SubscriptionInterceptor`), and the `lint/` analyzer; plus `relay/`, `fed/`, `ext/throttle`,
`ext/trusted` and DataLoader tracing. Not built: `ext/authz` (the Apollo directive vocabulary
and a batched `Guard`) and APQ over WebSocket, which belongs in the
`graphql-transport-ws` state machine. `@defer`/`@stream` is not merely unbuilt — the prelude's
`@defer` is stripped in `introspection.go` so the validator and introspection agree the
server says no; adding it reverses a decision rather than filling a gap. Not measured, both
needing Linux: latency percentiles, which this machine's ~522us clock granularity makes
impossible, and behaviour under a cgroup memory limit.

Keep this section honest. It said "subscription load testing not yet built" while the
architecture section above described `transport/gqlws/load_test.go` in detail, and called
`Manifest`/`AutoBind` unimplemented while documenting both — a file that contradicts itself
is worse than one that says nothing, because a reader trusts the half that happens to agree
with them.
