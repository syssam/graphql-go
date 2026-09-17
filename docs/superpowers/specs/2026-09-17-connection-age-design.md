# Connection age and idle limits — design

## Problem

A WebSocket or SSE connection lives as long as its client keeps it. Behind a load balancer that
means connections opened before a scale-out stay on the old instances indefinitely, and a
connection nobody uses holds its goroutines and socket for ever. grpc bounds both with
`keepalive.ServerParameters`: `MaxConnectionAge` (plus ±10% jitter so connections opened together
do not expire together), `MaxConnectionAgeGrace`, and `MaxConnectionIdle`, all unlimited by default.
Nothing in this repository has an equivalent; the only time-based closes are the init timeout and
the shutdown drain.

## API

WebSocket, in `internal/gqlwsproto.Config` and as options on both drivers:

```go
gqlws.WithMaxConnectionAge(age, grace time.Duration) Option
gqlws.WithMaxConnectionIdle(d time.Duration) Option
gqlfiber.WithMaxConnectionAge(age, grace time.Duration) Option   // WS only
gqlfiber.WithMaxConnectionIdle(d time.Duration) Option           // WS only
```

SSE:

```go
gqlsse.WithMaxStreamAge(d time.Duration) Option
gqlfiber.WithMaxStreamAge(d time.Duration) Option                // SSE only
```

`gqlecho.WS`/`SSE` forward `gqlws`/`gqlsse` options and need nothing. All default to zero, meaning
no limit. A new `internal/jitter.Spread(d)` returns `d` moved uniformly within ±10%.

## Semantics

### WebSocket age

Counted from `Serve` starting. At `jitter.Spread(age)` the connection drains exactly as a
shutdown drain does, but for this connection alone:

- new `subscribe` messages get a terminal `error`:
  `[{"message":"The connection has reached its maximum age."}]`;
- subscriptions end without `complete`;
- queries and mutations in flight run to completion;
- then close **1001** `Going away` — graphql-ws clients reconnect, which is the point.

If `grace > 0` and operations are still running `grace` after the drain began, the socket is
closed 1001 anyway. `grace == 0` waits for them, as grpc does.

### WebSocket idle

A connection is idle when it has no operation in flight — subscriptions count as in flight, as
grpc counts streams. Idleness is measured from the handshake completing, or from the moment the
last in-flight operation ended. Pings, pongs and a `complete` for an unknown id do not reset it.
When it has been idle for `d` it closes with **1000** `Idle timeout`: nothing was lost, and a
client reconnects when it next needs to. A `subscribe` that races the idle close is refused with
`[{"message":"The connection was idle."}]`.

### SSE stream age

Each subscription stream ends at `jitter.Spread(d)` after it opened, without a `complete` event, so
the client reconnects. Single-result requests are unaffected. There is no idle or grace: a stream
is one subscription, never idle while open, and ending it loses nothing in flight.

## Design

- `internal/gqlwsproto`:
  - `Serve` starts an age timer when `MaxConnectionAge > 0` and hands its channel to `watch`.
  - `watch` gains a case for it that calls `drain` with the age reason and, if grace is set, a
    grace timer channel.
  - `drain(parent, served, reason, grace)` stores the reason under `mu` beside `draining`, and adds
    a `grace` case to its wait that closes 1001.
  - `subscribe` refuses with the stored reason when draining (or "The server is shutting down." when
    `Closing` is closed but the watcher has not yet set `draining`).
  - Idle uses one `time.AfterFunc` timer created at the end of a successful handshake. `subscribe`
    stops it when the operation count goes from 0 to 1. `forget`/`stop` reset it when a deletion
    actually removed an id and left the count at 0 and the connection is not draining.
    `closeIfIdle` re-checks under `mu`, marks the connection draining with the idle reason, and
    closes 1000. `serve`'s defer marks draining and stops the timer before `cancelAll`, so teardown
    deletions cannot re-arm it.
  - New constant `StatusNormalClosure = 1000`.
- `gqlsse` / `gqlfiber` SSE: the streaming `select` gains an age timer case that returns without
  `complete`.

## Testing

Timing tests run in `testing/synctest`, so hours of fake time are exact and instant. Each is
broken on purpose once and must fail.

1. `jitter.Spread`: 10,000 samples all in [0.9d, 1.1d], not all equal; zero and negative returned
   unchanged.
2. Age drains: a subscription is open; the socket is still open at 0.9·age minus a minute and
   closed 1001 by 1.1·age plus a minute, with no `complete`.
3. Age lets a query finish, with grace long enough: `next` and `complete` precede the 1001.
4. Grace cuts: a query that never returns; closed 1001 by 1.1·age + grace + a minute; the query is
   then released so the test leaks nothing.
5. A `subscribe` after the age drain began gets the age error.
6. Idle closes 1000 after `d` with nothing in flight, not before.
7. Idle does not fire while a subscription is open; after the client completes it, it fires `d`
   later, not earlier.
8. Zero options: a connection stays open for 100 hours of fake time.
9. `gqlws` and `gqlfiber.WS`: the options reach `Serve` — one test each for age (1001) and idle
   (1000) over a real connection.
10. `gqlsse` and `gqlfiber.SSE`: a stream ends without `complete` after the age; single-result
    requests are unaffected.

## Out of scope

- Server-sent keepalive enforcement (grpc's `EnforcementPolicy`).
- Per-operation age: an age bounds connections, not individual subscriptions on a WebSocket.
