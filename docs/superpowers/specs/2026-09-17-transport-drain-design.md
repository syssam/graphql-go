# Draining long-lived connections on shutdown — design

## Problem

`net/http`'s `Server.Shutdown` (under `gqlhttp`, `gqlsse`, `gqlws` and all of `gqlecho`) and
fasthttp's `ShutdownWithContext` (under `gqlfiber`) already drain ordinary requests: they stop
accepting and wait for in-flight handlers. They do not drain the two long-lived kinds this
repository serves, and their documentation says so:

- **WebSocket** (`gqlws`, `gqlecho.WS`, `gqlfiber.WS`). `net/http`: "Shutdown does not attempt to
  close nor wait for hijacked connections such as WebSockets. The caller of Shutdown should
  separately notify such long-lived connections of shutdown". Today the process exits under the
  connection: no 1001 close frame, and a mutation in flight is cut with no signal to the client
  whether it ran. `gqlfiber`'s sockets are hijacked with `KeepHijackedConns`; nothing found shows
  fasthttp waits for them either (not measured).
- **SSE subscription streams** (`gqlsse`, `gqlecho.SSE`, `gqlfiber.SSE`). A stream is an active
  request that never ends by itself, so `Shutdown` always waits out its whole context and the
  stream is then cut by process exit.

No handler has a way to be told that shutdown has begun, which is the gap.

## API

A new package `transport/drain`:

```go
// Drain coordinates the shutdown of long-lived connections across handlers.
type Drain struct{ /* unexported */ }

func New() *Drain

// Shutdown starts draining and waits for every entered connection to leave.
// If ctx ends first, every connection's context is cancelled and Shutdown
// returns ctx.Err() without waiting further. Safe to call more than once and
// concurrently.
func (d *Drain) Shutdown(ctx context.Context) error

// Enter registers one long-lived connection or stream. ctx derives from parent
// and is cancelled if Shutdown gives up; leave must be called exactly once
// when the connection ends (extra calls are no-ops). ok is false once draining
// has begun: the caller must refuse the connection, and ctx and leave are
// then parent and a no-op.
func (d *Drain) Enter(parent context.Context) (ctx context.Context, leave func(), ok bool)

// Closing is closed when Shutdown begins.
func (d *Drain) Closing() <-chan struct{}
```

A nil `*Drain` is valid: `Enter` admits everything with `parent` and a no-op `leave`, `Closing`
returns nil (never ready in a select), `Shutdown` returns nil. Handlers without the option hold a
nil `*Drain` and behave exactly as today.

Options: `gqlws.WithDrain(d)`, `gqlsse.WithDrain(d)`, `gqlfiber.WithDrain(d)`. `gqlecho.WS` and
`gqlecho.SSE` already take `gqlws`/`gqlsse` options, so they need nothing. `gqlhttp` gets no option:
the server already drains it. `gqlfiber.WithDrain` affects only `gqlfiber.WS` and `gqlfiber.SSE`.

Usage, `net/http`:

```go
d := drain.New()
mux.Handle("/graphql/ws", gqlws.New(exec, gqlws.WithDrain(d)))
mux.Handle("/graphql/stream", gqlsse.New(exec, gqlsse.WithDrain(d)))
// on signal, run both; srv.Shutdown waits for SSE handlers, which return only once d drains them
var wg sync.WaitGroup
wg.Go(func() { _ = d.Shutdown(ctx) })
_ = srv.Shutdown(ctx)
wg.Wait()
```

## Semantics

### WebSocket (`internal/gqlwsproto`, driven by `gqlws` and `gqlfiber.WS`)

`gqlwsproto.Config` gains `Closing <-chan struct{}`. Once it is closed:

1. **Subscriptions** are cancelled without a `complete`, the same as a connection ending: the client
   is about to see 1001 and will resubscribe on reconnect, and a `complete` would tell it the
   subscription ended for good.
2. **Queries and mutations in flight** run to completion and send their `next` and `complete` as
   usual. This is the difference from cutting the connection: a mutation's client learns its result.
3. **A `subscribe` arriving while draining** gets a terminal `error` message
   (`[{"message":"The server is shutting down."}]`) and starts nothing.
4. When every operation has finished, the connection closes with **1001 "Going away"**
   (`StatusGoingAway`). graphql-ws clients treat 1001 as retryable.
5. A connection still in its `connection_init` handshake has nothing in flight and is closed with
   1001 at once.

If the connection's context is cancelled from outside while it is being served (which the drain does
when `Shutdown` gives up), the socket is closed with 1001 immediately. This matters for `gqlfiber`,
whose `Read` cannot be interrupted by a context: closing the socket is the only way to end a parked
read, which is already how the protocol ends connections.

Drivers:
- `gqlws.ServeHTTP` calls `Enter(context.Background())` before `websocket.Accept`; refused, it
  answers `503 Service Unavailable` without upgrading. It passes the entered context to `Serve`
  (replacing `context.Background()`), `Closing()` in the config, and defers `leave`.
- `gqlfiber.WS` checks `Closing()` before upgrading and answers 503 if it is closed. Inside the
  upgraded callback it calls `Enter`; refused there (the race between the check and the upgrade), it
  closes the socket with 1001. Otherwise as `gqlws`.

### SSE (`gqlsse`, `gqlfiber.SSE`)

Only subscription streams enter the drain. A single-result request (query or mutation, streamed as
one `next` and `complete`) is an ordinary request that the server already waits for.

- Before opening the subscription, the handler calls `Enter(requestContext)`; refused, it answers
  `503` with the handler's ordinary error body. The entered context is what `Subscribe` runs under.
- The streaming loop also selects on `Closing()`; when it fires the loop returns **without writing
  `complete`**, ending the response. The client reconnects.
- `leave` is deferred to the end of the stream (for `gqlfiber`, inside the stream writer's existing
  cleanup).

## Why these choices

- **One shared object, not a `Shutdown` method per handler.** `gqlfiber.WS`/`SSE` and `gqlecho.*`
  return handler functions, which cannot carry a method. One `Drain` for the whole server also
  matches grpc's `GracefulStop`: one call, every connection.
- **Hard cut on the deadline, then return.** A graceful wait that never ends is the failure this
  exists to fix; waiting after the deadline would recreate it. grpc likewise falls back from
  `GracefulStop` to `Stop`.
- **Queries and mutations finish, subscriptions do not wait.** A subscription has no natural end to
  wait for; a mutation does, and cutting it leaves the client unsure whether it applied.

## Testing

Every test is broken on purpose once and must fail.

1. `drain`: `Shutdown` with nothing entered returns nil at once; it waits for an entered connection
   and returns when `leave` runs; `Enter` after `Shutdown` began is refused; a deadline cancels every
   entered context and returns `ctx.Err()` without waiting for `leave`; `leave` twice is harmless;
   nil `*Drain` admits, never closes, and shuts down to nil. Under `-race`.
2. `gqlwsproto` (fake socket and a real executor): on `Closing`, a subscription stops with no
   `complete` and the socket closes 1001; a query blocked in a resolver is allowed to finish — its
   `next` and `complete` are written before the close; a `subscribe` sent after `Closing` gets an
   `error` and the close still follows; a connection still in handshake closes 1001; cancelling the
   Serve context closes the socket 1001.
3. `gqlws` over a real server: after `d.Shutdown` the client's open subscription ends with close
   1001 and `Shutdown` returns nil well before its deadline; a new connection gets 503.
4. `gqlsse` over a real server: an open subscription stream ends without a `complete` event after
   `d.Shutdown`; `srv.Shutdown` run alongside returns before its deadline; a new subscription gets
   503; a subscription stream without the option is unaffected by an unrelated drain.
5. `gqlfiber.WS` and `gqlfiber.SSE`: the same two behaviours as 3 and 4 through Fiber.
6. `gqlecho`: one test that `gqlecho.WS(exec, gqlws.WithDrain(d))` closes 1001 on drain, proving the
   pass-through.

## Out of scope

- `gqlhttp`: already drained by the server.
- Retry-After headers on the 503: no transport sets them today, and a client reconnect policy is
  the client's.
- A drain for the executor itself (in-flight operations outside any transport). Callers of
  `Executor.Execute` own their own lifetimes.
