---
paths:
  - "transport/**"
  - "internal/httpreq/**"
  - "internal/gqlwsproto/**"
  - "internal/jitter/**"
---

# Transports, drain and connection limits

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
**`httpreq.Decode` skips leading JSON whitespace itself**: `gqlhttp` and `gqlfiber` trimmed
before calling it (they look for a batch `[` first) and the SSE handlers did not, so
`" {...}"` was 200 over HTTP and 400 over SSE (`TestEquivalence/leading whitespace`).

**A client that names neither JSON type gets `application/json`.** An empty `Accept` and a
wildcard both answer `MediaTypeJSON`, and with it the 200 that media type carries for a
request error: the client never opted into `application/graphql-response+json`, so it should
not be handed the status that goes with it. The specification's audit suite requires both
rows ("SHOULD accept */* and use application/json for the content-type", "SHOULD assume
application/json content-type when accept is missing") and graphql-http, graphql-yoga and
Apollo Server all answer `application/json` to a wildcard. An explicit type still outranks a
wildcard in either order. A `q` outside RFC 9110's qvalue grammar (`q=O`, `q=2`, `q=1e0`)
makes that range not acceptable, as `q=0` does; it used to count as `q=1`, promoting what
was probably a typo for a refusal to first choice. `internal/httpreq.TestNegotiate` is the whole table, which
`Negotiate` had no direct test for until this changed. **`gqlhttp` takes 61 of 61 on that
audit** with CSRF prevention off, 58 with it on, where the three are `MAY` rows sending a GET
with no preflight header. See `testdata/httpaudit/`.

**A missing `query` member is 400 whatever was negotiated**, because a body that carried no
query never became an operation -- it is a malformed request, not a GraphQL request error, and
only the latter follows the media type. `execute` returns that as a second result rather than
letting the status rule see it. With persisted queries on the check happens after APQ
resolution, and until this was fixed it went through the media-type rule there: an
`application/json` client got 200 for a request `httpreq` answers 400 when APQ is off, so the
same request had two answers depending on an unrelated option.

**A response that fails to serialize still gets a body.** The status header is already on the
wire when `WriteTo` fails -- the only thing that can fail is an extension value
`encoding/json` will not take -- so the status cannot be corrected, but an empty 200 is
indistinguishable from success to any client and the operator sees only a warning.
`httpreq.WriteBody` counts what was written and the handler appends
`{"errors":[{"message":"internal system error"}]}` **only when nothing was**, which is what
keeps a failure *after* bytes went out -- a client disconnecting mid-write -- from corrupting a
partly-written response. It is safe because `Response.WriteTo` composes the whole envelope
before writing any of it. Both SSE handlers do the same through `writePayload`, for a
pre-stream request error (their `writeResponse`) and for a `next` event, which used to go
out as `data: ` with nothing after it and end the stream without `complete`.

Losing the envelope is the reference behaviour, not a shortcoming: graphql-js, run for
comparison, is `JSON.stringify` semantics -- a function or `undefined` is silently dropped, a
BigInt or a cycle throws and the whole response is lost. Go has no drop tier, so every one of
those is an error here. Answering with *nothing at all* is the part that had no precedent.

**`DefaultCSRFHeaders` is an interop surface, and its bar is one property.** A header belongs
on it when a browser cannot put it on a cross-origin request without a preflight -- that is,
when it is outside the CORS safelist (`Accept`, `Accept-Language`, `Content-Language`,
`Content-Type`, `Range`). Every name that clears that bar costs nothing and admits a client
that would otherwise see a 403 it has no way to distinguish from the server being down. The
list carried the specification's `GraphQL-Require-Preflight` and the pre-CORS
`X-Requested-With` but not Apollo Server's `Apollo-Require-Preflight`, which is the one Apollo
Client sends -- so the most widely deployed GraphQL client was refused from a GET, the request
shape automatic persisted queries are built on. `WithCSRFPrevention`'s variadic headers
*replace* the list rather than extend it, so narrowing it is deliberate.
`TestEquivalence/every default CSRF header is accepted` drives every name in the list through
every handler, so adding one without wiring it fails rather than being assumed.

**The APQ path nothing drove was a miss over GET.** Every APQ test registered the query first,
so `writeGraphQLError` -- the only caller of which is the persisted-query miss on the GET
branch -- was at 0% in `gqlhttp` and `gqlfiber` both. It is the *first* request an Apollo
client ever sends.

**A persisted query is resolved once per request.** The GET branch resolves before the
mutation-over-GET check, so it passes `resolved` to `execute` in `gqlhttp` and `gqlfiber`.
Resolving twice was harmless for APQ, which re-verified the hash of the text it had just
loaded, and fatal for a safelist, which refuses any request carrying text: every registered
id over GET, the CDN-cacheable shape a safelist is for, came back `PersistedQueryNotInList`.
Every safelist test sent text or used POST ("safelisted id over GET executes").

**An APQ retry handshake is 200 under every media type** (`apq.IsRetryHandshake`, consulted at
all four status decisions in `gqlhttp` and `gqlfiber`). This used to follow the media type like
any other request error -- 200 for `application/json`, 400 for
`application/graphql-response+json` "because that is what the specification says" -- and a
differential against Apollo Server 5.5.1 showed both halves of that reasoning were wrong.
Automatic persisted queries are not in the specification at all; they are Apollo's protocol,
and Apollo answers `PersistedQueryNotFound` with 200 whatever was negotiated. Apollo Client
sends `Accept: application/graphql-response+json`, and `*/*` and a missing `Accept` reach the
same branch here, so the 400 was what real clients got on every cold cache -- the exact failure
the `application/json` case was written to avoid. `CodeNotInList` is excluded: a safelist
refusal is a rejection, and retrying with the text is what it forbids. Both codes and the
safelist case are asserted across the HTTP family.

An unknown *hash* against a `TrustedStore` still answers `PersistedQueryNotFound`, not
`PersistedQueryNotInList`, so a safelist now invites a retry it will then refuse. One wasted
round trip, no weaker a safelist -- but it is why the safelist row sends query text.

`gqlecho` is `net/http` underneath, so it delegates to `gqlhttp`/`gqlsse`/`gqlws` rather than
reimplementing them; its only addition over `echo.WrapHandler` is raising an
`*echo.HTTPError` for **every error status but 400** (`reportable`) so Echo's error handler
and middleware see it; the body is already written and stays the GraphQL envelope. It was an
allow-list of 403/405/406/415, which let 413, a drain's 503 and `gqlsse`'s 500 pass as
successes. 400 stays unraised because it is the status of a GraphQL request error (validation
under `application/graphql-response+json`, every request error on SSE) -- a response the
client asked for -- and the recorder sees only the status, so a malformed body's 400 cannot be
told apart from it. `gqlfiber` is fasthttp-native instead: parsing
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
so a panicking `OnConnect` does not leave it parked. Two drain divergences are deliberate and
known: `gqlws` checks the drain before anything else, so a non-upgrade GET while draining gets
503 where `gqlfiber` answers 426, then 403 (origin), then 503; and drain refusals are not in
`transport/equivalence_test.go`. Two guards in `gqlwsproto` have no deterministic
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

**`fakeSocket` cannot fail a write**, so for a long time nothing in `gqlwsproto` reached any
write-failure branch; `failWriteSocket` (`writefail_test.go`) fails exactly one write and then
behaves, which is what lets a test see what the connection does after losing a message rather
than after losing all of them. The guard worth knowing about is `c.stop(id)` in `run`'s
write-failure branch, and **it is not what releases the source** — subscribe launches the pump
as `go func() { defer cancel(); c.run(...) }()`, so the source is freed however `run` returns.
What only `stop` does is delete the entry from `c.subs`, and while that entry is there the
connection believes an operation is in flight: it never goes idle, the id cannot be reused,
and the slot stays counted against `MaxSubs`. Removing it leaves every other test in this
package green and fails only `TestCancelledConnectContextCloses` in `transport/gqlws`, which
is about a different guard entirely. `TestWriteFailureRetiresTheSubscription` holds it
directly, using idle under `synctest` as the observable because polling for a `MaxSubs`
refusal races the pump and reads whichever answer arrives first.

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

`transport/gqlws/load_test.go` opens 150 concurrent subscriptions and requires both the
source registrations and the goroutines back afterwards; it honours `-short`. **`GQLWS_LOAD_CONNS` raises the count** -- 150 is what the Windows development machine's
ephemeral ports allow, not a limit of the engine. Run at 1 000, 10 000 and 20 000 on Linux
(`--ulimit nofile=200000`): exactly one goroutine per subscription, every one returned to a
3-goroutine baseline, and 30-37 KB per connection for client and server together, flat across
the whole range. The leak threshold stays absolute (`baseline+10`) rather than scaling, so a
larger run is stricter per connection; the waits scale with the count, because 20 000 clients
take longer to register than 150 and a fixed deadline would report that as a leak.
`BenchmarkSubscriptionFanout` measures a broadcast reaching every subscriber by counting
receipts, because `publish` drops rather than blocks and timing it alone reports a fan-out
to 128 clients at 45ns each when the honest figure is 4us. **Releasing an operation is
doubly redundant** — the operation context derives from the connection context, and
`cancelAll` also calls each stored cancel — so breaking either leaves every test green and
only breaking both leaks. Do not read a green suite as evidence that one of them is dead.

In `gqlwsproto`, **writes use the connection context, never the operation's**: coder/websocket
tears down the whole connection when a write context is cancelled mid-frame, so writing a
`next` under the operation context would let one client's unsubscribe drop every other
subscription on that connection. The init timeout likewise closes the connection from a
timer rather than bounding the read, because a read aborted by its own context leaves no
way to send the 4408 close frame. **The ack and that timer decide under one lock
(`initDeadline`)** before the ack is written: the timer used to be stopped only by
`handshake`'s defer, so one firing while the ack write blocked closed an authenticated
connection 4408, and one firing during a slow `OnConnect` closed the socket and the handshake
acknowledged it anyway. `ack`'s `timer.Stop` is only cleanup -- the `acked` check in `fire` is
what answers, so breaking the Stop alone changes nothing observable.

**Why `gqlfiber` is native rather than `adaptor.HTTPHandler(gqlhttp...)`**, and it has nothing
to do with allocations (the cost comparison is in `docs/benchmarks.md`): `fasthttpadaptor`
hands the wrapped handler a `*fasthttp.RequestCtx` as its request context, and
`RequestCtx.Done()` is documented as the **server's** shutdown channel, not a per-request one —
so a `gqlhttp` handler reached through the adaptor never sees a client disconnect, and every
in-flight adapted request sees `ctx.Err() != nil` the moment shutdown begins.
`benchmarks/k6/` load tests the HTTP transports against `benchmarks/cmd/transportserver`; read
its README before quoting a number from it, because the timings it first recorded were single
samples and have been retracted.
