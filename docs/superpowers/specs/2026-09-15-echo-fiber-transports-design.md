# Echo v5 and Fiber v3 Transports: Design Specification

- **Date:** 2026-09-15
- **Branch:** `transport-echo-fiber` (cut from `phase4-otel`)
- **Status:** Approved design; not yet implemented
- **Verified against:** `github.com/labstack/echo/v5 v5.3.1`,
  `github.com/gofiber/fiber/v3 v3.5.0`,
  `github.com/gofiber/contrib/v3/websocket v1.2.6`

## 1. Background

`transport/` currently holds three packages — `gqlhttp`, `gqlsse` and `gqlws` —
all built on `net/http`. Nothing in the repository references Echo, Fiber or
fasthttp. Users of those frameworks are therefore in two very different
positions:

- **Echo v5 is `net/http`.** `echo.WrapHandler(gqlhttp.New(exec))` already
  works today for all three transports. What is missing is an idiomatic
  handler signature, error mapping into Echo's error handler, and any test
  proving the combination behaves.
- **Fiber is fasthttp.** There is no `http.ResponseWriter` anywhere in a Fiber
  request. The only route today is `adaptor.HTTPHandler`, which rebuilds a
  synthetic `*http.Request` and buffers the response per call. WebSocket
  subscriptions do not work that way at all: `coder/websocket` requires
  `http.Hijacker`, which fasthttp does not provide.

This specification adds first-class support for both, and treats the
performance claim as something to be measured rather than asserted.

## 2. Goals and Non-Goals

### Goals

1. `transport/gqlecho` and `transport/gqlfiber`, each serving queries,
   mutations and subscriptions over HTTP, SSE and WebSocket.
2. A single implementation of the client-visible HTTP rules shared by every
   transport, extended rather than duplicated.
3. A single implementation of `graphql-transport-ws`, driven by two different
   socket libraries.
4. A measured performance comparison across `net/http`, Echo, Fiber-native and
   Fiber-via-`adaptor`, reported through `benchstat`.
5. Self-contained examples for each framework.

### Non-Goals

- Framework support beyond Echo v5 and Fiber v3. Gin, Chi and anything else
  `net/http`-based already works through `gqlhttp`; this branch does not add
  packages for them.
- Echo v4. v5 is a distinct, incompatible API (see section 3.1) and is what was
  requested.
- Fiber v2. v3 is current; v2's `*fiber.Ctx` pointer receiver would require a
  second set of adapters for no stated benefit.
- APQ over WebSocket, which CLAUDE.md already records as unbuilt and is
  orthogonal to this work.

## 3. Two findings that shape the design

These were established by reading the released modules, not from memory, and
each one changes what gets built.

### 3.1 Echo v5 is not Echo v4

In v4, `echo.Context` is an interface and `c.Response()` returns
`*echo.Response`. In v5:

```go
type HandlerFunc func(c *Context) error   // *Context is a struct, not an interface
func (c *Context) Request()  *http.Request
func (c *Context) Response() http.ResponseWriter
func (c *Context) Logger()   *slog.Logger
func WrapHandler(h http.Handler) HandlerFunc
```

`Request()` and `Response()` hand over exactly what the three existing
handlers need, so `gqlecho` is genuinely thin. `Logger()` returning
`*slog.Logger` lines up with the existing `WithLogger` option, so an Echo
application's logger can be threaded through without an adapter type.

### 3.2 Fiber's context never cancels

From the `fiber.Ctx` interface documentation:

> `Ctx` satisfies `context.Context` as a context that can never be canceled
> ... `Done` always returns `nil`.

and `c.Context()` returns "a non-nil, empty context" unless the application
set one. Only `c.RequestCtx()` carries a deadline.

This matters more here than it would in most libraries. The engine uses
context cancellation as its teardown mechanism:

- `Executor.Subscribe` returns a channel closed when the source closes **or
  the context is cancelled**;
- in-flight concurrent resolvers unwind through the context under the bounded
  semaphore.

A Fiber transport that passes `c` or a bare `c.Context()` into the executor
therefore **leaks every subscription for the lifetime of the process** and
never cancels resolver work when a client disconnects. Nothing about this is
visible in a functional test that runs to completion; it needs a test written
specifically for it. Section 4.4 covers the remedy.

## 4. Architecture

Four units, each with one purpose.

### 4.1 `internal/httpreq` — widen the accessor, keep one rule set

The package's own doc comment states its purpose:

> It exists so that `gqlhttp` and `gqlsse` cannot drift apart on rules a
> client can tell the difference between. A request rejected as forgeable by
> one transport must be rejected by the other.

Every exported function currently takes `*http.Request`, so a fasthttp
transport cannot call any of them. Rather than let `gqlfiber` re-implement the
CSRF check, the body limit and JSON decoding — which is exactly the drift the
package was created to prevent — the package gains a minimal accessor:

```go
// Source is the request surface the transports share.
type Source interface {
    Method() string
    Header(name string) string
    QueryParam(name string) string
    Body(max int64) ([]byte, error)
}
```

`Forgeable`, `ParseGET` and `RequireJSONBody` operate on `Source`. `Decode` and
`IsJSONObject` already take bytes and strings and are unchanged. Two adapters
are provided: one over `*http.Request` (used by `gqlhttp`, `gqlsse`,
`gqlecho`) and one over `fiber.Ctx` (used by `gqlfiber`). The fasthttp adapter
lives in `gqlfiber` so that `internal/httpreq` itself keeps importing only the
standard library.

The `queryOptional` flag that APQ depends on is preserved verbatim, so that
with APQ off the missing-query errors stay byte-identical, as CLAUDE.md
requires.

**Known divergence, to be closed by test.** `ReadBody` currently uses
`http.MaxBytesReader`, which both caps the read and marks the connection as
poisoned. Fiber reads the body eagerly into memory before the handler runs, so
the cap is enforced by `fiber.Config.BodyLimit` plus a length check in the
adapter. The mechanisms differ; the client-visible result must not. A table
test asserts the status code and error text for an over-long body are
identical across all four HTTP-carrying transports.

### 4.2 `internal/gqlwsproto` — one protocol, two sockets

`transport/gqlws/conn.go` (387 lines) implements the `graphql-transport-ws`
state machine directly against `coder/websocket`. Fiber cannot use that
library, so without a refactor the protocol would be written twice — two
places to fix every protocol bug.

The state machine moves to `internal/gqlwsproto`, parameterised over a socket:

```go
// Socket is the transport under the protocol.
type Socket interface {
    Read(ctx context.Context) ([]byte, error)
    Write(ctx context.Context, data []byte) error
    Close(code int, reason string) error
}
```

Split of responsibilities:

- **Stays in the driver** (`gqlws`, `gqlfiber`): handshake and upgrade,
  `websocket.AcceptOptions`, origin patterns, read limits expressed in the
  library's own terms.
- **Moves to `gqlwsproto`**: message framing and dispatch, `connection_init`
  and the init timeout, ping/pong, subscription registry and the
  max-subscriptions cap, `OnConnect`, close codes.

Two invariants must survive the move intact. Both are recorded in CLAUDE.md as
past bug fixes, and both are easy to lose in a refactor:

1. **Writes use the connection context, never the operation's.** A write
   cancelled mid-frame tears down the whole connection, so writing a `next`
   under the operation context would let one client's unsubscribe drop every
   other subscription on that connection.
2. **The init timeout fires from a timer, not by bounding the read.** A read
   aborted by its own context leaves no way to send the 4408 close frame.

**Execution constraint:** this lands as a standalone refactor commit, with
`transport/gqlws/conn_test.go` unmodified and passing. If those tests require
editing to go green, the refactor has changed behaviour and is backed out
rather than patched. This is the highest-risk change on the branch.

### 4.3 `transport/gqlecho`

A thin `net/http` passthrough exposing `echo.HandlerFunc` for each transport,
constructed from the same options as the underlying handlers. Transport-level
failures are mapped to `*echo.HTTPError` so that an application's
`HTTPErrorHandler` and middleware chain observe them, which is the single
behavioural reason to prefer this package over `echo.WrapHandler`.

GraphQL errors are *not* mapped to `echo.HTTPError`: a GraphQL response
carrying field errors is a successful HTTP response, and the status rules for
`application/graphql-response+json` already live in `gqlhttp`. Only failures
that occur before or outside a GraphQL response — a rejected method,
unacceptable `Accept`, a forgeable request — become `HTTPError`.

Expected performance: within benchmark noise of raw `gqlhttp`. A measurable
gap indicates a defect in the wrapper, not a cost of Echo.

### 4.4 `transport/gqlfiber`

Fasthttp-native throughout; `adaptor` is used only as a benchmark comparison
point, never on the serving path.

- **Parsing** goes through the shared `httpreq` rules via the fasthttp
  `Source` adapter.
- **Writing** uses `resp.WriteTo(c)`. `fiber.Ctx` implements
  `Write([]byte) (int, error)` and `graphql.Response` already implements
  `WriteTo(io.Writer)`, so the response JSON goes straight into the fasthttp
  response buffer with no intermediate allocation and no `net/http`
  conversion. This preserves the project's "writes JSON straight into a pooled
  buffer" property across the new transport.
- **SSE** uses `c.SendStreamWriter(func(w *bufio.Writer))`, flushing per event.
- **WebSocket** uses `github.com/gofiber/contrib/v3/websocket` v1.2.6 (over
  `fasthttp/websocket`) as a `gqlwsproto.Socket` driver.

**Mind the WebSocket module path.** `github.com/gofiber/contrib/websocket`
— the path most documentation and search results point at — is **Fiber v2
only**; its `go.mod` pins `fiber/v2 v2.52.6` and it will not build against v3.
The Fiber v3 module is the differently-shaped path
`github.com/gofiber/contrib/v3/websocket`, whose v1.2.6 pins `fiber/v3 v3.5.0`
and `fasthttp/websocket v1.5.12`. That release is dated 2026-09-09, six days
before this spec, so it is both the only option and a young one: see the risk
table in section 8.

**Cancellation.** Every handler derives a cancellable context rather than
passing Fiber's own:

```go
ctx, cancel := context.WithCancel(c.Context())
defer cancel()
```

and cancels it when the peer is known to be gone. For unary requests the
handler's return is sufficient. For SSE, a failed `w.Flush()` is the only
disconnect signal fasthttp offers, so it drives `cancel()`. For WebSocket, the
socket's read error or close drives it. Without this the executor never learns
the client left; see section 3.2.

### 4.5 Module layout

Everything stays in the root module. CLAUDE.md's dependency rule is explicitly
"about what the root package imports, not about module purity", and it already
notes that `ext/otel` is "the obvious candidate to split into its own module at
publication time" — establishing that the current practice is one module with
splitting deferred. Framework adapters are the same class of dependency and
follow the same rule.

The deciding factor is the test gate: `go vet ./... && go test -race ./...` is
described as "the standard gate for every change", and it stops covering these
packages the moment they leave the module. Weakening the gate on the branch
that introduces two fasthttp transports and a subscription-protocol refactor
is the wrong trade. When `ext/otel` is split at publication time, `gqlecho`
and `gqlfiber` split alongside it.

Cost accepted: the root `go.sum` grows by roughly sixteen modules, fasthttp
among them.

## 5. Examples

Two self-contained examples, each with its own SDL, resolvers and `main`, each
serving HTTP, SSE and WebSocket:

- `examples/echo/`
- `examples/fiber/`

Self-contained is deliberate: an example is most useful when it can be copied
wholesale as a starting point, without tracing shared files.

## 6. Testing

Test-driven throughout: each behaviour below gets a failing test before the
code that satisfies it.

### 6.1 Shared conformance

Echo and Fiber are held to the client-visible behaviour already pinned for
`gqlhttp`, `gqlsse` and `gqlws`, by reusing those expectations rather than
writing new ones. A transport that passes its own bespoke assertions but
differs from `gqlhttp` is the failure mode being guarded against.

### 6.2 Cross-transport equivalence

A table test drives the same set of requests through all four HTTP transports
and asserts identical status and body for:

- an over-long body (the `MaxBytesReader` vs `BodyLimit` divergence, 4.1);
- a forgeable request under CSRF prevention;
- a non-JSON `Content-Type` on POST;
- a mutation attempted over GET;
- a missing `query` member, with APQ both on and off.

### 6.3 Protocol refactor

`transport/gqlws/conn_test.go` passes unmodified after the `gqlwsproto`
extraction. Fiber's WebSocket driver is then held to the same protocol
expectations.

### 6.4 Fiber cancellation

A test that opens a Fiber SSE subscription, drops the client, and asserts the
executor's context is cancelled and the subscription goroutine exits. Written
first, and observed to fail against a naive implementation that passes
`c.Context()` through — a test that has never failed proves nothing here.

### 6.5 Race detection

The full gate is `go vet ./... && go test -race ./...`. `-race` matters
particularly for `gqlfiber`: `SendStreamWriter` runs the flush loop on a
different goroutine from the executor's resolvers.

## 7. Performance suite

Lives in the `benchmarks/` module, which is already separate and already
carries gqlgen.

**Matrix.** `net/http` + `gqlhttp` (baseline), Echo v5, Fiber-native,
Fiber-via-`adaptor`. The `adaptor` row exists to test the premise of 4.4: if
the native path does not beat the bridge by a worthwhile margin, that is a
finding to report, not to bury.

**Two harnesses.** In-process handler invocation for `allocs/op` and `B/op`,
and a real loopback listener for end-to-end `ns/op` — fasthttp's advantages
are in connection and header handling, which an in-process benchmark does not
exercise at all.

**Method.** `-count=10 -benchmem`, compared with `benchstat`. CLAUDE.md records
single samples on this codebase being wrong by 20-77% on a warm machine, so
single-sample numbers are not reported.

Results are written to `docs/benchmarks.md` alongside the existing gqlgen
comparison.

## 8. Risks

| Risk | Mitigation |
|---|---|
| `gqlwsproto` extraction silently changes subscription behaviour | Standalone commit; `conn_test.go` unmodified and green, or the refactor is reverted |
| Fiber cancellation gap reappears in a future handler | A cancellation test per Fiber transport, not one for the package |
| `httpreq` widening changes an existing error message | Cross-transport equivalence table (6.2); existing `gqlhttp`/`gqlsse` tests unmodified |
| Fiber-native turns out not to beat `adaptor` | Reported as a result; the `adaptor` row is in the matrix precisely so this is answerable |
| Root `go.sum` growth is judged unacceptable | Split to separate modules is mechanical and reversible; 4.5 records the decision and its cost |
| `gofiber/contrib/v3/websocket` is six days old and the only Fiber v3 websocket option | Confined behind `gqlwsproto.Socket`, so replacing it touches one driver file and no protocol code. If it proves unusable, Fiber ships HTTP + SSE — which already serve every operation kind — and WebSocket follows when the dependency matures |

## 9. Deliverables

1. `internal/httpreq` widened to `Source`; `gqlhttp` and `gqlsse` migrated.
2. `internal/gqlwsproto` extracted; `gqlws` migrated as a pure refactor.
3. `transport/gqlecho` — HTTP, SSE, WebSocket.
4. `transport/gqlfiber` — HTTP, SSE, WebSocket, fasthttp-native.
5. `examples/echo/` and `examples/fiber/`.
6. Benchmark matrix in `benchmarks/`, results in `docs/benchmarks.md`.
7. CLAUDE.md updated: the Transports section, and the dependency note in
   Conventions.
