# Running it

What to set, what to watch, and what this engine will not do for you.

Every number here is measured or is a shipped default; where something has not
been measured, the last section says so rather than leaving it to be assumed.

## Before it faces anything untrusted

**Four limits are off by default.** An engine that enables none of them can be
made to do arbitrary work by one unauthenticated request — that is a documented default, not a bug, and
`SECURITY.md` says the same. Turning them on is the deployment's job because
the right numbers depend on the schema.

| Option | Default | Set it to |
|---|---|---|
| `WithMaxDepth` | off | Deeper than your deepest real query, not much more. Checked from the document before a plan is built, so a refused query costs one AST walk. |
| `WithMaxComplexity` | off | Same. Both are structural and cheap. |
| `WithQueryCost` | off | `Max` against measured traffic; set `Report: true` first and watch `extensions.cost` for a week before enforcing. |
| `WithOperationTimeout` | off | It bounds work, not latency: a resolver that ignores its context still holds the response. |
| `WithMaxResponseBytes` | **64 MiB** | On already. Lower it if your largest legitimate response is far smaller. |
| `WithMaxTokens` | **15 000** | On already. Parsing and validation run before every limit above, on every document, valid or not; see "Document limits" below. |
| `WithMaxNesting` | **100** | On already. Selection sets and input literals as written, not after fragments. |
| `WithMaxErrors` | **1000** | On already. |
| `WithPlanCacheBytes` | **16 MiB**, with `WithPlanCache` at **1024** entries | On already, and it is what stops distinct large queries from retaining gigabytes: a parsed document retains roughly 26x its query text. |
| `apq.WithMaxBytes` | **16 MiB** | On already. Any client can register any query it can hash, so an entry count alone lets 1000 entries of 1 MiB park a gigabyte. |
| `gqlws.WithWriteTimeout` / `gqlfiber.WithWriteTimeout` | **10 s** | On already. It is what ends a WebSocket peer that stays connected but stops reading; pings do not. |

### Document limits

Parsing and validation happen before `WithMaxDepth`, `WithMaxComplexity` and
`WithQueryCost` can refuse anything, and before a timeout starts to matter,
and three of gqlparser's default validation rules are super-linear in the
document. An external review measured, with every limit above set:

| Shape | Size | Unbounded | Now |
|---|---|---|---|
| One field repeated | 10 KB | 1.2 s, 1.25 GB | refused past 15 000 tokens; 10 ms at the largest accepted |
| Aliases with differing arguments | 43 KB | 2.0 s, 1.24 GB (an error per pair) | 4 ms at the largest accepted, errors cut at `WithMaxErrors` |
| Nested list literal | 1 MiB | 3 min 17 s | refused past 100 levels |
| `{not:{not:...}}` on a recursive input | 60 KB | 13 s, 17 GB | 57 ms at the largest accepted |
| Fragment DAG | 51 KB | 5.8 s | refused past the fragment budget; 83 ms at the largest accepted |

Four things bound it:

- **`WithMaxTokens`** stops the parser, gqlgen's and Apollo Router's default.
- **`WithMaxNesting`** caps selection sets and input literals as written.
- **Above 256 selections, OverlappingFieldsCanBeMerged is replaced**
  (`overlap.go`) by the specification's FieldsInSetCanMerge, checked per
  response name rather than per pair, which is linear. It agrees with
  gqlparser's rule on every graphql-js spec case and on 20 000 random
  documents (`overlap_test.go`); below 256 gqlparser's runs, for its exact
  messages.
- **A fixed fragment budget** refuses a document whose fragments spread one
  another so often that gqlparser's walker, which finds each spread's
  definition by scanning the fragment list, would take more than about
  50 ms. A 511-fragment tree, a large Relay screen's shape, uses 7% of it.

`TestValidationIsBoundedByTheDefaultLimits` builds the largest document each
shape can have under the defaults and requires it to validate in
milliseconds. Raising either limit raises what one request can cost; lifting
both (zero) leaves the parser's recursion bounded only by the request body.

Two more that are not limits but belong in the same review:

- **`RequireAuthCoverage()`** if the schema declares authorization at all. It
  fails the build for a field that declares neither a requirement nor
  `@public`. What it catches is not the field someone forgot to guard — those
  get noticed — but a type nobody noticed arriving.
- **`WithRecover`** is on by default. A panicking resolver becomes
  `internal system error` with code `INTERNAL_SERVER_ERROR`; the panic value
  and stack go to `slog` and never to the client. A panicking DataLoader
  batch function fails every `Load` waiting on it the same way, and a panic
  in a subscription event's interceptors or presenter becomes one error event
  rather than ending the process.

## Shutdown

`http.Server.Shutdown` does not end long-lived connections. Its own
documentation says so — it "does not attempt to close nor wait for hijacked
connections such as WebSockets" — and an SSE stream is an ordinary request that
never returns, so `Shutdown` waits out its whole deadline and the process then
cuts the stream mid-flight.

Hand one `drain.Drain` to `gqlws`, `gqlsse` and `gqlfiber` through `WithDrain`,
and **shut it down alongside the server, never before or after**:

```go
var wg sync.WaitGroup
wg.Go(func() { d.Shutdown(shutdownCtx) })
srv.Shutdown(shutdownCtx)   // waits for SSE handlers that only the drain ends
wg.Wait()
```

Draining first, then calling `Shutdown`, is not equivalent: `Shutdown` has to
be running for the handlers to return into it. Draining after `Shutdown`
returns is worse — `Shutdown` waits out its whole deadline first, because the
streams it is waiting for are the ones only the drain ends.
`examples/storefront` and `examples/blog` both get this right; copy from there
rather than from memory.

What a drain does on `Closing`: a WebSocket refuses new operations with an
`error`, cancels subscriptions **without** `complete`, lets queries and
mutations finish so their clients learn the result, then closes 1001. An SSE
subscription stream returns without a `complete` event. Omitting `complete` is
deliberate — it would tell graphql-ws and graphql-sse clients the subscription
ended for good instead of reconnecting.

## What sizes a replica

- **The schema is built once and retained.** 11.0 MB at 200 entities
  (~1 600 types), against 0.04 MB for gqlgen, which does that work at
  generation time. This is the design's standing cost and it is per process,
  not per request. A memory-bound deployment fits fewer replicas per node
  because of it.
- **The plan cache** is bounded by entry count and by query text
  (`WithPlanCacheBytes`, 16 MiB).
- **Per request** the engine allocates 12 to 14 objects for a small query
  (measured, `docs/performance.md`) and does not grow with schema size — that is the whole point of the compiled
  plan, and it is why request memory is not what you tune.
- **A response over ~15.7 MB is not pooled.** `jsonw` drops a buffer whose
  capacity passes 16 MiB, and `append` overshoots, so the next request rebuilds
  it from scratch at about twice the bytes. The cap was 4 MiB, then 8, and each
  raise came from measuring full introspection rather than reasoning about it:
  at 4 MiB a 3.30 MB response cost 38.4 MB/op where a 2.83 MB one cost 16.6, and
  at 8 MiB a real 5 516-type schema's 6.56 MB introspection cost 71.3 MB/op on
  every request where it costs 31.5 once it fits. **Size your headroom against
  your own introspection response, not against your type count** -- that schema
  is 1.25 KB per type where the synthetic one used to set the cap was 0.79. If
  your responses routinely pass 15 MB, [`performance.md`](performance.md) has
  the numbers to raise it against.
- **Goroutines grow with the request, not with `WithMaxConcurrency`.** Every
  concurrently scheduled field and list element runs on its own goroutine, so
  a resolver returning 10 000 elements with resolver fields starts 10 000. The
  option is a slot budget that `Stats` reports, and a task never waits for a
  slot: one that did would hold back the DataLoader wave its siblings are
  parked in. Bound the size of a request instead, with `WithQueryCost`,
  `WithMaxComplexity` and `WithMaxResponseBytes`.

## What to watch

Register the runtime instruments after the executor and the drain exist, and
**each exactly once per meter** — two registrations of one executor add every
value into the same series and silently double it.

```go
exec := graphql.NewExecutor(s, otel.New()...)  // spans and the request layer
reg, err := otel.ObserveExecutor(exec)         // plan cache, concurrency
regD, err := otel.ObserveDrain(d)              // long-lived connections
// reg.Unregister() / regD.Unregister() on shutdown.
```

| Instrument | Alert when |
|---|---|
| `graphqlgo.loader.keys` | **one key per span.** That is DataLoader batching degraded to N+1, and it is the single most useful alarm here. |
| `graphqlgo.plan_cache.lookups` split by `graphqlgo.plan_cache.result` | the miss ratio climbs — the cache is too small, or clients send text that varies per request. |
| `graphqlgo.plan_cache.bytes` / `.count` | at the ceiling continuously. |
| `graphqlgo.executor.concurrency.in_use` vs `.limit` | pinned at the limit — resolvers are the bottleneck, not the engine. |
| `graphqlgo.transport.active_connections` | grows without bound, or does not fall during a drain. |
| `graphql.server.active_requests` | queries and mutations only; subscriptions never appear here. |

`graphql.server.*` names are the generic concept, `graphqlgo.*` this engine's.
There is no per-operation dimension, deliberately, to keep cardinality bounded.
The instruments carry no identity of their own, so use `otel.WithAttributes` to
tell two executors on one meter apart.

Field spans (`WithFieldSpans`) are opt-in and cost a `tracer.Start`, a
`SetAttributes` and a `span.End` on **every** field including pure ones. Leave
them off in steady state.

## Authorization in production

- **`WithObjectAuthBatch` should be sized against what one policy call costs in
  latency, not against what the backend accepts in one request.** Batches are
  issued one after another, so latency multiplies by their number: a
  1 000-row list at the default batch of 50 is twenty sequential round trips —
  100 ms added to one field at 5 ms each, measured. Halving the batch doubles
  it.
- **A guarded object behind a single-object field is one call carrying one
  check.** A list of 100 rows each reaching one guarded child is 2 batched
  calls plus 100 single ones. Model the position as a list, or cache inside the
  policy; the batch size cannot help there.
- **A failing policy backend fails closed and stops asking.** A backend that
  errors on its third batch of twenty makes three calls, writes no rows, and
  does not spend the remaining seventeen round trips on a backend already known
  to be down.
- The `Authorizer` runs once per query or mutation and **once per subscription
  event**, so a scope revoked mid-stream applies to the next event.

## Limits you have to design around

These are decisions, not gaps, and each is cheaper to know now than to discover
in an incident.

- **No `@defer` / `@stream`.** The prelude's `@defer` is stripped so that the
  validator and introspection agree the server says no. Clients that rely on
  incremental delivery — Relay in particular — need another plan.
- **On a subscription, an instance check withholds the payload, not the
  event.** A subscriber who may not see a row still receives an event carrying
  the refusal, so timing leaks where contents do not. Filter in the resolver
  that opens the stream if that matters.
- **`gqlfiber` with `WithKeepAlive(0)`** leaves an idle SSE subscription with
  no write that can fail, so it is held open until its source ends or
  `WithMaxStreamAge` ends it. The handler warns at construction.
- **`gqlfiber`'s WebSocket sets no read deadline.** The only candidate interval
  is `PingInterval`, and the protocol tracks no pongs, so a derived deadline
  would drop slow-but-live clients.
- **An HTTP batch of n runs n operations in sequence**, and
  `WithOperationTimeout` bounds each, not the batch.
- **A connection age rotation is not free for a busy client.** An operation
  sent while the age drain waits for a long query gets a terminal `error`,
  which graphql-ws does not retry, and that happens every age period rather
  than only at shutdown.

## In a container

Measured on 2026-09-24 with `benchmarks/cmd/memlimit` in `golang:1.27`, under
`docker run --memory=256m --memory-swap=256m` -- **both** flags, because
`--memory` alone grants an equal amount of swap and the process then survives
well past its limit. A Kubernetes pod has no swap, so `--memory` on its own
measures something no deployment sees.

**Go reads the cgroup CPU limit and not the memory limit.** In a container
limited to 2 CPUs and 256 MiB, the runtime reports:

```
GOMAXPROCS=2 NumCPU=20
GOMEMLIMIT=9223372036854775807   (unset)
/sys/fs/cgroup/memory.max = 268435456
```

`GOMAXPROCS` follows `cpu.max` on its own. Nothing sets `GOMEMLIMIT`, so the
collector has no idea a limit exists and grows until the kernel kills the
process.

**Set `GOMEMLIMIT`.** The same workload, 4 concurrent requests in a 256 MiB
container:

| response | `GOMEMLIMIT` unset | `GOMEMLIMIT=200MiB` |
|---:|---|---|
| 5.5 MB | survives, 33 GCs | survives, 38 GCs |
| 11.1 MB | survives, 34 GCs | survives, 32 GCs |
| **16.9 MB** | **killed (exit 137)** | **survives, 136 GCs** |

At 16.9 MB responses the unlimited runtime is OOM-killed on a workload it can
otherwise serve. `GOMEMLIMIT` converts the kill into collector pressure: it
survives, at four times the GC count and roughly twice the wall time. That is
the trade, and it is the right one -- a slow server is a server.

**`GOMEMLIMIT` does not raise the floor.** Concurrent responses are live at the
same time, and no collector can reclaim what is in use:

| live bytes (response × concurrency) | 256 MiB container, `GOMEMLIMIT=200MiB` |
|---:|---|
| 16.9 MB × 4 = 68 MB | survives |
| 22.7 MB × 4 = 91 MB | survives |
| 11.1 MB × 8 = 89 MB | survives |
| 34.2 MB × 4 = 137 MB | killed |
| 11.1 MB × 16 = 178 MB | killed |

The boundary on this shape sits between 91 MB and 137 MB of concurrent response
bytes in a 256 MiB container -- call it a third of the limit, and note it is one
shape on one machine. **So size `WithMaxResponseBytes` against the container,
not the machine.** Its default is 64 MiB: four concurrent responses at that
size are 256 MiB, which is the whole container. The default exists to stop one
runaway query, not to make a small container safe.

**A CPU limit is also a memory limit here.** The same 16 concurrent requests
that are killed at `GOMAXPROCS=20` survive at 4 and at 2:

| `--cpus` | GOMAXPROCS | 11.1 MB × 16 |
|---:|---:|---|
| 20 | 20 | killed |
| 4 | 4 | survives, peak heap 211 MB |
| 2 | 2 | survives, peak heap 209 MB |

`WithMaxConcurrency` defaults to 4 × GOMAXPROCS, so capping CPU caps the
engine's own fan-out and with it how much response is live at once. **Many CPUs
with little memory is the dangerous combination**, and it is the one a
generously-sized node with a small pod limit produces by default.

One reading trap: `runtime.MemStats.Sys` goes above the container limit (305 MB
in a 256 MiB container) on runs that survive comfortably. That is reserved
address space, not resident pages; the cgroup accounts RSS.


**Sizing a subscription fleet.** One goroutine per subscription, and 30-37 KB
per connection measured from 150 up to 20 000 with no superlinear term -- but
that figure counts the load generator's client too, so treat it as an upper
bound on the server's share (see
[`performance.md`](performance.md#subscriptions-at-scale)). 20 000 subscribers
held 590 MB of heap for both sides together, which against the container numbers
above means a subscription server wants its memory limit sized on connection
count, and `GOMEMLIMIT` set, before it wants anything else.

## What has not been measured

Stated so that nobody reads silence as a result.

- **Everything in `docs/performance.md` was measured on Windows** on one
  developer machine. CI builds and tests on Linux and macOS; it does not
  benchmark them.
- ~~Latency percentiles.~~ Measured on Linux, where the clock tick is 17 ns
  rather than this machine's 211 µs; see
  [`performance.md`](performance.md#latency-percentiles). Still unmeasurable on
  Windows.
- ~~Behaviour under a cgroup memory limit.~~ Measured; see
  [In a container](#in-a-container) above.
- ~~Schema build cost above ~1 600 types.~~ Measured; see
  [`performance.md`](performance.md). 4 800 types build in 78 ms and retain
  33 MB, and the variable is the width of the widest type rather than the
  count -- narrowing a 4 800-field Query root to 100 halves it.
- **No production hours.** This engine has not served a real request outside a
  benchmark. Treat every figure here as a laboratory result until your own
  traffic says otherwise.
