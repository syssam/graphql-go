---
paths:
  - "ext/**"
  - "fed/**"
  - "relay/**"
  - "stats.go"
---

# Extensions: otel, apq, trusted, throttle, fed, relay

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
carry no identity of their own**: two executors on one meter add every value into one series,
and one executor observed twice doubles every value, silently — review measured the first
version's gauges overwriting each other instead — hence `otel.WithAttributes` and "observe each once per meter" in the godoc. No per-operation
dimension, to keep cardinality bounded. `graphql.server.active_requests` wraps `reqChain`, which
only `Execute` runs, so subscriptions never show there; `graphqlgo.transport.active_connections` is
where they do.

`otel.Batch` / `otel.MappedBatch` wrap a `loader.BatchFunc` so every flush gets a span under
the request that caused it. They live in `ext/otel` rather than as a loader option so
`loader/` keeps depending on nothing but the engine and the standard library.
`graphqlgo.loader.keys` is the attribute to alert on: one key per span means batching has
degraded to N+1.

`ext/apq` is automatic persisted queries, opt-in through `WithPersistedQueries` on the HTTP
transports and, on a subscribe message, on `gqlws` and `gqlfiber`. **A WebSocket miss goes out
as `next` then `complete`, never as `error`**: the whole handshake depends on the client
reading `PersistedQueryNotFound` and retrying with the full text, and graphql-ws treats
`error` as terminal, so sending one silently breaks the protocol this implements. That is why
it does not go through `runOnce`, which routes a response carrying request errors to
`finishWithErrors`. `internal/gqlwsproto` takes it as `Config.ResolvePersisted`, a func rather
than an `apq.Cache`, so the protocol core keeps depending on nothing but the root package
while the transports — which already import `ext/apq` for their HTTP handlers — supply it.
**`apq.IsRetryHandshake` is the HTTP side of the same rule**, and the HTTP side had it wrong
until a differential against Apollo Server 5.5.1: the miss followed the negotiated media type
like any other request error, so a client accepting `application/graphql-response+json` — which
is every Apollo Client — got a 400 it reports as a failed request rather than retrying. The
WebSocket carrier never made that mistake. See `transports.md`.
On both carriers **resolution happens while the request is still being read, not at
execution** — on a WebSocket before the id is registered or a subscription slot is taken, and
over HTTP during parsing. The HTTP ordering is load-bearing: a request carrying
only a hash has no query text, so the "mutations are not allowed over GET" guard would have
nothing to inspect and would wave a persisted mutation through. Registration verifies
`sha256(query) == hash` — storing whatever text arrived would let one client choose what
every later client's hash executes. `httpreq` takes a `queryOptional` flag so that with APQ
off the missing-query errors are byte-identical to before. **`apq.NewCache` is bounded by
query text as well as entry count** (`WithMaxBytes`, 16 MiB by default): registration is open
to any client that can hash, so an entry count alone let 1000 entries of 1 MiB hold a gigabyte.
**A hash is lower-cased before lookup and verification** (hex is case-insensitive and `Hash`
writes lower case), but **not against a `TrustedStore`**, whose ids are opaque build ids as
often as digests (`TestHashCaseIsIgnored`, `TestIDsAreMatchedExactly`).

`ext/trusted` is the same wiring as a safelist: a `Store` is an `apq.Cache`, and `apq.Resolve`
tells the two apart by the `apq.TrustedStore` marker. A safelist must refuse query text
however it hashes — verifying the hash only proves the client can hash — and must refuse a
freeform request carrying no hash at all, which is otherwise the way straight round it.
**That wiring is per transport**: a `gqlws` or `gqlsse` mounted without `WithPersistedQueries`
runs anything. `Store.Enforce()` is the executor-level boundary every transport shares -- a
request interceptor (refuses before parsing) plus a subscription interceptor, because
`Subscribe` runs no request interceptor -- matching on document *text*, so a hash a transport
resolved from the store passes. Its limits, from what an interceptor can reach: a subscription
is refused only after parse/validate/plan, so it can still learn a validation error, and
`OperationKind` parses every document a streaming transport sees into the document cache.
Closing both needs a root hook that runs on the raw request for `Subscribe` and
`OperationKind` as well as `Execute`.

`ext/throttle` spends what `QueryCost` computes: a bucket of points per caller, refilled on
read rather than on a timer, quoted before the query runs and refunded down to the actual
cost after. **It must be the first operation interceptor, and nothing checks that**: an
`ExecutorOption` is an opaque `func(*Executor)` and the chain is unexported, so enforcing it
needs a root API (an ordering hook, or a read-only view of the registered interceptors). **It charges per subscription event**, because each event runs the whole
operation chain; a stream billed once at open is unmetered. A quote is clamped at zero before
it is charged: a negative `FieldWeight` makes a negative quote, and charging one added points.

`fed/` makes a schema an Apollo Federation subgraph and needs no engine change: `_entities`
is an ordinary root field returning a union, and a union resolves its concrete type from the
dynamic Go type, the same path `Query.node` takes. The prelude adds only protocol
scaffolding no author writes in any implementation, and `_service` returns the author's text
verbatim because that text is what the router composes from. **Only object types with `@key`
are `_Entity` members** — a union's members must be objects, and an interface carrying `@key`
is reached through its implementing types, so a resolver for one is rejected at build time.
`fed.BatchResolver` exists because `_entities` is the canonical N+1: the router sends every
representation in one call, `fed.Resolver` resolves them in turn, and a DataLoader inside it
cannot coalesce sequential `Load`s.

`relay/` binds the Relay contract: global ids (`base64("Type:id")`, byte for byte what
graphql-relay-js and graphql-java produce), `Node`, and cursor connections. It emits no
types — the SDL still declares `Node`, the connection and the edge, as in every reference
implementation. `Query.node` needed no engine change: it binds through the existing
`any`-returning resolver on an unbound interface.
