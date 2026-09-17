# Transport Drain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let WebSocket and SSE handlers wind down on shutdown through one shared `drain.Drain`: subscriptions end (1001 / stream end without `complete`), in-flight WebSocket queries and mutations finish, new connections get 503, and `Shutdown(ctx)` cuts everything when its deadline passes.

**Architecture:** A small `transport/drain` package tracks entered connections. `internal/gqlwsproto` gains a `Closing` channel and does the WebSocket protocol work once for both WebSocket drivers. Each handler takes a `WithDrain` option; `gqlecho` inherits it through the options it already forwards.

**Tech Stack:** Go 1.27, `coder/websocket`, Fiber v3 / `fasthttp/websocket`, Echo v5.

**Spec:** `docs/superpowers/specs/2026-09-17-transport-drain-design.md`

## Global Constraints

- Root package unchanged; `transport/drain` imports only the standard library.
- A handler without `WithDrain` behaves exactly as before (nil `*drain.Drain` is valid everywhere).
- WebSocket close on drain is code 1001, reason `Going away`; exported as `StatusGoingAway` in `gqlwsproto`, `gqlws` and `gqlfiber`.
- Error text for refusals and drain-time subscribes is exactly `The server is shutting down.`; refusal status is 503.
- Subscriptions ended by drain send no `complete` (WebSocket) and no `complete` event (SSE).
- `gqlwsproto` writes stay under the connection context, never an operation context (existing rule).
- `-race` on every run. Every new test is broken on purpose once and must fail; undo with a reverse edit, never `git checkout -- <file>`.
- Tests that wait use explicit timeouts and never `time.Sleep` for correctness except where the plan says "give it a moment" to prove something has *not* happened.
- Comments explain why, English only. Commits: lower-case type prefix, imperative, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Gate for each task: `go vet ./... && go test -race -count=1 ./<touched packages>/...`; the final task runs `sh scripts/gate.sh`.

---

### Task 1: `transport/drain`

**Files:** Create `transport/drain/drain.go`, `transport/drain/drain_test.go`.

**Interfaces — Produces:** `func New() *Drain`; `func (d *Drain) Enter(parent context.Context) (ctx context.Context, leave func(), ok bool)`; `func (d *Drain) Closing() <-chan struct{}`; `func (d *Drain) Shutdown(ctx context.Context) error`.

- [ ] **Step 1: Tests** — `transport/drain/drain_test.go`:

```go
package drain_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/syssam/graphql-go/transport/drain"
)

func TestShutdownWithNothingEnteredReturnsAtOnce(t *testing.T) {
	d := drain.New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	select {
	case <-d.Closing():
	default:
		t.Fatal("Closing is not closed after Shutdown")
	}
}

// TestShutdownWaitsForLeave pins the graceful half: Shutdown returns only once
// every entered connection has left, and the connection's context stays alive
// while it winds down on its own.
func TestShutdownWaitsForLeave(t *testing.T) {
	d := drain.New()
	ctx, leave, ok := d.Enter(context.Background())
	if !ok {
		t.Fatal("Enter refused before any Shutdown")
	}
	done := make(chan error, 1)
	go func() { done <- d.Shutdown(context.Background()) }()

	select {
	case <-d.Closing():
	case <-time.After(time.Second):
		t.Fatal("Closing was not closed when Shutdown began")
	}
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned %v before the connection left", err)
	case <-time.After(50 * time.Millisecond):
	}
	if ctx.Err() != nil {
		t.Fatal("a draining connection's context was cancelled before the deadline")
	}
	leave()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after the connection left")
	}
}

func TestEnterRefusedOnceDraining(t *testing.T) {
	d := drain.New()
	_ = d.Shutdown(context.Background())
	parent := context.Background()
	ctx, leave, ok := d.Enter(parent)
	if ok {
		t.Fatal("Enter admitted a connection after Shutdown began")
	}
	if ctx != parent {
		t.Fatal("a refused Enter must hand back parent")
	}
	leave() // must be a harmless no-op
}

// TestShutdownDeadlineCutsConnections pins the hard half: past the deadline
// every entered context is cancelled and Shutdown returns without waiting for
// connections that never leave.
func TestShutdownDeadlineCutsConnections(t *testing.T) {
	d := drain.New()
	ctx, leave, _ := d.Enter(context.Background())
	defer leave()

	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := d.Shutdown(sctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the entered context was not cancelled when Shutdown gave up")
	}
}

func TestLeaveTwiceIsHarmless(t *testing.T) {
	d := drain.New()
	_, leave, _ := d.Enter(context.Background())
	leave()
	leave() // a second Done on the WaitGroup would panic
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
}

func TestNilDrainAdmitsEverything(t *testing.T) {
	var d *drain.Drain
	parent := context.Background()
	ctx, leave, ok := d.Enter(parent)
	if !ok || ctx != parent {
		t.Fatal("a nil Drain must admit with parent")
	}
	leave()
	if d.Closing() != nil {
		t.Fatal("a nil Drain's Closing must be nil so a select never fires on it")
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Shutdown = %v", err)
	}
}
```

- [ ] **Step 2: See them fail** — `go test -race ./transport/drain/` → build failure (package has no non-test files).

- [ ] **Step 3: Implement** — `transport/drain/drain.go`:

```go
// Package drain winds down long-lived connections when a server shuts down.
//
// net/http's Server.Shutdown and fasthttp's ShutdownWithContext wait for
// ordinary requests, but not for hijacked WebSockets, and an SSE subscription
// never ends on its own, so Shutdown waits out its whole deadline for one.
// A Drain is handed to every handler that serves such connections (see
// gqlws.WithDrain, gqlsse.WithDrain and gqlfiber.WithDrain) and shut down
// alongside the server:
//
//	d := drain.New()
//	mux.Handle("/graphql/ws", gqlws.New(exec, gqlws.WithDrain(d)))
//	...
//	var wg sync.WaitGroup
//	wg.Go(func() { _ = d.Shutdown(ctx) })
//	_ = srv.Shutdown(ctx)
//	wg.Wait()
//
// The two run together because srv.Shutdown waits for SSE handlers, which
// return only once the drain has ended their streams.
package drain

import (
	"context"
	"sync"
)

// Drain tracks the long-lived connections of one server. A nil *Drain is
// valid and does nothing, which is what a handler without the option holds.
type Drain struct {
	mu       sync.Mutex
	draining bool
	closing  chan struct{}
	wg       sync.WaitGroup

	// force is cancelled when Shutdown gives up, and every entered context
	// follows it.
	force  context.Context
	giveUp context.CancelFunc
}

// New returns a Drain that admits connections until Shutdown is called.
func New() *Drain {
	force, giveUp := context.WithCancel(context.Background())
	return &Drain{closing: make(chan struct{}), force: force, giveUp: giveUp}
}

// Enter registers one long-lived connection or stream. The returned context
// derives from parent and is also cancelled if Shutdown's deadline passes;
// leave must be called when the connection ends, and calling it again does
// nothing. Once draining has begun Enter reports ok false, hands back parent
// and a no-op leave, and the caller must refuse the connection.
func (d *Drain) Enter(parent context.Context) (ctx context.Context, leave func(), ok bool) {
	if d == nil {
		return parent, func() {}, true
	}
	// The draining check and the Add share the lock with Shutdown's flip, so
	// no Add can race the Wait that follows it.
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		return parent, func() {}, false
	}
	d.wg.Add(1)
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(d.force, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			cancel()
			d.wg.Done()
		})
	}, true
}

// Closing is closed when Shutdown begins. It is nil for a nil Drain, which a
// select never chooses.
func (d *Drain) Closing() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.closing
}

// Shutdown stops admitting connections, signals Closing, and waits for every
// entered connection to leave. If ctx ends first it cancels every entered
// context and returns ctx.Err() at once rather than waiting further: a wait
// that outlives its deadline is the failure this exists to prevent. It may be
// called more than once and concurrently.
func (d *Drain) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.draining {
		d.draining = true
		close(d.closing)
	}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		d.giveUp()
		return ctx.Err()
	}
}
```

- [ ] **Step 4: Green** — `go vet ./transport/drain/ && go test -race -count=1 ./transport/drain/`.

- [ ] **Step 5: Breaks** (reverse each):
  1. In `Enter`, delete the `if d.draining {...}` block → `TestEnterRefusedOnceDraining` fails.
  2. In `Shutdown`, replace `d.giveUp()` with nothing → `TestShutdownDeadlineCutsConnections` fails.
  3. In `leave`, remove `once.Do` wrapping (call the three lines directly) → `TestLeaveTwiceIsHarmless` panics.
  4. In `Shutdown`, return nil immediately after closing `closing` → `TestShutdownWaitsForLeave` fails.

- [ ] **Step 6: Commit** — `feat: add a drain that winds down long-lived connections on shutdown`.

---

### Task 2: WebSocket protocol drain (`internal/gqlwsproto`)

**Files:** Modify `internal/gqlwsproto/config.go`, `conn.go`, `protocol.go`; create `internal/gqlwsproto/drain_test.go`.

**Interfaces — Produces:** `Config.Closing <-chan struct{}`; `const StatusGoingAway = 1001`. Consumes nothing from Task 1 (the protocol takes a channel so it stays independent of the drain package).

- [ ] **Step 1: Tests** — `internal/gqlwsproto/drain_test.go` (reuses `fakeSocket`, `newFakeSocket`, `types`, `closeCode` from `conn_test.go`):

```go
package gqlwsproto

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
)

// drainSource backs a schema with one query that blocks until released and
// one subscription fed by the test.
type drainSource struct {
	entered chan struct{}
	once    sync.Once
	release chan struct{}
	ticks   chan int
}

func newDrainExecutor(t *testing.T) (*drainSource, *graphql.Executor) {
	t.Helper()
	src := &drainSource{entered: make(chan struct{}), release: make(chan struct{}), ticks: make(chan int)}
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { block: String! }
		type Subscription { tick: Int! }
	`),
		graphql.Query(graphql.Resolve("block", func(context.Context, graphql.Root) (string, error) {
			src.once.Do(func() { close(src.entered) })
			<-src.release
			return "done", nil
		})),
		graphql.Subscription(graphql.Subscribe("tick", func(context.Context) (<-chan int, error) {
			return src.ticks, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return src, graphql.NewExecutor(s)
}

// serveDraining starts Serve with a Closing channel and returns it with a
// channel closed when Serve returns.
func serveDraining(t *testing.T, ctx context.Context, sock *fakeSocket, exec *graphql.Executor) (closing chan struct{}, done chan struct{}) {
	t.Helper()
	closing, done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		Serve(ctx, sock, Config{Exec: exec, InitTimeout: 5 * time.Second, MaxSubs: 10, Closing: closing})
	}()
	return closing, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestDrainEndsSubscriptionWithGoingAway: a subscription is cancelled without
// a complete, which would tell the client not to resubscribe, and the
// connection closes 1001.
func TestDrainEndsSubscriptionWithGoingAway(t *testing.T) {
	src, exec := newDrainExecutor(t)
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)
	sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"subscription { tick }"}}`)
	closing, done := serveDraining(t, context.Background(), sock, exec)

	src.ticks <- 1
	waitFor(t, "the first next", func() bool { return slices.Contains(sock.types(), "next") })

	close(closing)
	waitDone(t, done)
	if got := sock.closeCode(); got != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, StatusGoingAway)
	}
	if slices.Contains(sock.types(), "complete") {
		t.Fatalf("drain sent complete for a subscription: %v", sock.types())
	}
}

// TestDrainLetsQueryFinish: a query in flight is not cut. Its next and
// complete are written, and only then does the connection close.
func TestDrainLetsQueryFinish(t *testing.T) {
	src, exec := newDrainExecutor(t)
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)
	sock.in <- []byte(`{"id":"q","type":"subscribe","payload":{"query":"{ block }"}}`)
	closing, done := serveDraining(t, context.Background(), sock, exec)

	<-src.entered
	close(closing)
	time.Sleep(50 * time.Millisecond) // give a wrong implementation the chance to close early
	if got := sock.closeCode(); got != 0 {
		t.Fatalf("connection closed with %d while a query was still running", got)
	}
	close(src.release)
	waitDone(t, done)

	got := sock.types()
	if n := len(got); n < 2 || got[n-2] != "next" || got[n-1] != "complete" {
		t.Fatalf("message types = %v, want the query's next and complete last", got)
	}
	if code := sock.closeCode(); code != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", code, StatusGoingAway)
	}
}

// TestDrainRefusesNewSubscribe: while a query holds the connection open, a
// new operation is refused with an error rather than started.
func TestDrainRefusesNewSubscribe(t *testing.T) {
	src, exec := newDrainExecutor(t)
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)
	sock.in <- []byte(`{"id":"q","type":"subscribe","payload":{"query":"{ block }"}}`)
	closing, done := serveDraining(t, context.Background(), sock, exec)

	<-src.entered
	close(closing)
	sock.in <- []byte(`{"id":"late","type":"subscribe","payload":{"query":"subscription { tick }"}}`)
	waitFor(t, "an error for the late subscribe", func() bool { return slices.Contains(sock.types(), "error") })

	close(src.release)
	waitDone(t, done)
	if got := sock.closeCode(); got != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, StatusGoingAway)
	}
}

// TestDrainClosesHandshakingConnection: nothing is in flight before the
// handshake, so the close is immediate rather than waiting out InitTimeout.
func TestDrainClosesHandshakingConnection(t *testing.T) {
	_, exec := newDrainExecutor(t)
	sock := newFakeSocket()
	closing, done := serveDraining(t, context.Background(), sock, exec)
	close(closing)
	waitDone(t, done)
	if got := sock.closeCode(); got != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, StatusGoingAway)
	}
}

// TestCancelledContextClosesSocket: when the drain gives up it cancels the
// connection context. A driver whose Read ignores its context (gqlfiber) can
// only be unblocked by closing the socket, so Serve must do that.
func TestCancelledContextClosesSocket(t *testing.T) {
	_, exec := newDrainExecutor(t)
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)
	ctx, cancel := context.WithCancel(context.Background())
	_, done := serveDraining(t, ctx, sock, exec)
	waitFor(t, "the ack", func() bool { return slices.Contains(sock.types(), "connection_ack") })
	cancel()
	waitDone(t, done)
	if got := sock.closeCode(); got != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, StatusGoingAway)
	}
}
```

- [ ] **Step 2: See them fail** — build failure on `Closing` / `StatusGoingAway`.

- [ ] **Step 3: Implement.**

`protocol.go`, in the status const block, first entry:

```go
	// StatusGoingAway is RFC 6455's 1001, sent when the server is shutting
	// down. graphql-ws clients treat it as retryable.
	StatusGoingAway                = 1001
```

`config.go`, in `Config` after `OnConnect`:

```go
	// Closing, when closed, drains the connection: subscriptions end without
	// complete, queries and mutations in flight finish, new operations are
	// refused, and the connection then closes with StatusGoingAway. Optional.
	Closing <-chan struct{}
```

`conn.go`:

1. `conn` struct: add after `subs map[string]context.CancelFunc`:

```go
	// draining is set under mu, beside every wg.Add, so an operation either
	// starts before the drain waits or is refused.
	draining bool

	// streams parents every subscription and is cancelled by the drain;
	// queries and mutations do not derive from it, so they run to the end.
	streams     context.Context
	stopStreams context.CancelFunc
```

2. `Serve`: replace the body from `ctx, cancel := context.WithCancel(ctx)` to `c.serve()` with:

```go
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	streams, stopStreams := context.WithCancel(context.Background())
	defer stopStreams()

	c := &conn{
		cfg:         cfg,
		sock:        sock,
		ctx:         ctx,
		cancel:      cancel,
		subs:        make(map[string]context.CancelFunc),
		streams:     streams,
		stopStreams: stopStreams,
	}
	served := make(chan struct{})
	go c.watch(parent, served)
	c.serve()
	close(served)
```

3. Add after `Serve`:

```go
// watch ends the connection from outside the read loop, which is the only way
// to reach a Read that ignores its context. It watches parent rather than the
// connection's own context, which Serve cancels on its way out: only a cancel
// from the caller -- a drain giving up -- should close the socket.
func (c *conn) watch(parent context.Context, served <-chan struct{}) {
	select {
	case <-c.cfg.Closing:
		c.drain(served)
	case <-parent.Done():
		c.close(StatusGoingAway, "Going away")
	case <-served:
		// A cancelled parent also ends the read loop, so served can win the
		// race against parent.Done; the socket must still be closed then.
		if parent.Err() != nil {
			c.close(StatusGoingAway, "Going away")
		}
	}
}

// drain refuses new operations, ends subscriptions, waits for the rest, and
// closes. The close is what ends the read loop.
func (c *conn) drain(served <-chan struct{}) {
	c.mu.Lock()
	c.draining = true
	c.mu.Unlock()
	c.stopStreams()
	c.wg.Wait()
	select {
	case <-served:
		return // the connection ended on its own meanwhile
	default:
	}
	c.close(StatusGoingAway, "Going away")
}
```

4. `subscribe`: immediately after `c.mu.Lock()` insert

```go
	if c.draining {
		c.mu.Unlock()
		cancel()
		return c.writeError(msg.ID, graphql.Errorf("The server is shutting down.")) == nil
	}
```

and move `c.wg.Add(1)` to just before `c.mu.Unlock()` that follows `c.subs[msg.ID] = cancel`, so it reads:

```go
	c.subs[msg.ID] = cancel
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
```

5. `run`: replace

```go
	events, err := c.cfg.Exec.Subscribe(ctx, req)
```

with

```go
	// A subscription also ends when the connection drains; a query or
	// mutation above does not, so a client learns its result.
	ctx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	defer context.AfterFunc(c.streams, cancelStream)()
	events, err := c.cfg.Exec.Subscribe(ctx, req)
```

(After cancellation the existing `ctx.Err() != nil` checks in the loop and after it already return without `complete`.)

- [ ] **Step 4: Green** — `go vet ./internal/gqlwsproto/ && go test -race -count=1 ./internal/gqlwsproto/ ./transport/gqlws/ ./transport/gqlfiber/`.

- [ ] **Step 5: Breaks** (reverse each):
  1. In `drain`, delete `c.stopStreams()` → `TestDrainEndsSubscriptionWithGoingAway` times out.
  2. In `drain`, delete `c.wg.Wait()` → `TestDrainLetsQueryFinish` fails.
  3. In `subscribe`, delete the `if c.draining {...}` block → `TestDrainRefusesNewSubscribe` fails.
  4. In `watch`, delete the `case <-parent.Done():` branch → `TestCancelledContextClosesSocket` fails.
  5. In `run`, delete the `defer context.AfterFunc(...)()` line → `TestDrainEndsSubscriptionWithGoingAway` times out.

- [ ] **Step 6: Commit** — `feat: drain graphql-transport-ws connections with 1001 once operations finish`.

---

### Task 3: `gqlws` and `gqlecho.WS`

**Files:** Modify `transport/gqlws/handler.go`, `transport/gqlws/protocol.go`; add tests to `transport/gqlws/conn_test.go` (or a new `transport/gqlws/drain_test.go` in the same package) and `transport/gqlecho/stream_test.go` (or a new test file there).

**Interfaces:** Consumes Task 1 `drain.Drain` and Task 2 `gqlwsproto.Config.Closing`, `gqlwsproto.StatusGoingAway`. Produces `gqlws.WithDrain(d *drain.Drain) Option`, `gqlws.StatusGoingAway`.

- [ ] **Step 1: Tests.**
  - `TestDrainClosesWithGoingAway` (package `gqlws_test`, beside `dial`): `d := drain.New()`; `c := dial(t, e, gqlws.WithDrain(d))` using `newTestExecutor`; `c.init("")`; `c.subscribe("1", "subscription { messages { id } }")` (use the subscription root field `newTestExecutor`'s SDL actually declares — read `sdl` in that file); wait until the source has a registered subscriber the way `TestClientCompleteUnsubscribes` does, or send one event and `c.recv()` its `next`. Then run `d.Shutdown` with a 5s context in a goroutine recording its error and duration; assert `c.recvErr() == websocket.StatusGoingAway`; assert Shutdown returned nil in under 2s.
  - `TestDrainRefusesNewConnections`: build `srv := httptest.NewServer(gqlws.New(e, gqlws.WithDrain(d)))`, call `d.Shutdown(context.Background())` (returns nil, nothing entered), then `websocket.Dial` with the subprotocol; assert the error is non-nil and the returned `*http.Response` has `StatusCode == 503`.
  - `gqlecho`: one test that serves `gqlecho.WS(exec, gqlws.WithDrain(d))` through an Echo app exactly the way the existing gqlecho WebSocket test does, opens a subscription, calls `d.Shutdown`, and asserts close status 1001.

- [ ] **Step 2: See them fail** — build failure on `gqlws.WithDrain`.

- [ ] **Step 3: Implement.**

`protocol.go`: add `StatusGoingAway = gqlwsproto.StatusGoingAway` as the first entry of the status block.

`handler.go`: import `github.com/syssam/graphql-go/transport/drain`; add field `drain *drain.Drain` to `Handler`; add option

```go
// WithDrain registers every connection with d, so d.Shutdown winds them down:
// subscriptions end, queries and mutations in flight finish, and the
// connection closes with StatusGoingAway. Once d is draining, new connections
// are refused with 503.
func WithDrain(d *drain.Drain) Option { return func(h *Handler) { h.drain = d } }
```

In `ServeHTTP`, before `opts := h.accept`:

```go
	ctx, leave, ok := h.drain.Enter(context.Background())
	if !ok {
		http.Error(w, "The server is shutting down.", http.StatusServiceUnavailable)
		return
	}
	defer leave()
```

and in the `gqlwsproto.Serve` call replace `context.Background()` with `ctx` and add `Closing: h.drain.Closing(),` to the config. Keep the existing comment about why the connection has its own context, adjusted to say the context comes from the drain (still not from the request).

- [ ] **Step 4: Green** — `go test -race -count=1 ./transport/gqlws/ ./transport/gqlecho/`.

- [ ] **Step 5: Breaks:** remove `Closing: h.drain.Closing(),` → `TestDrainClosesWithGoingAway` fails (Shutdown times out, no 1001); replace the `!ok` branch with nothing (proceed to Accept) → `TestDrainRefusesNewConnections` fails.

- [ ] **Step 6: Commit** — `feat: drain gqlws connections through WithDrain`.

---

### Task 4: `gqlsse` and `gqlecho.SSE`

**Files:** Modify `transport/gqlsse/handler.go`; tests in `transport/gqlsse/` (new `drain_test.go`, package as the existing tests) and one in `transport/gqlecho/`.

**Interfaces:** Consumes `drain.Drain`. Produces `gqlsse.WithDrain(d *drain.Drain) Option`.

- [ ] **Step 1: Tests** (use `newTestExecutor`, `newServer`, `post`, `readEvents` from `handler_test.go`):
  - `TestDrainEndsStreamWithoutComplete`: open a subscription stream (as `TestSubscriptionStreamsEvents` does) on a server built with `gqlsse.WithDrain(d)`; publish one event and read its `next` from the body; call `d.Shutdown` (5s ctx) in a goroutine; read the remaining body to EOF (with a 5s deadline) and assert no `complete` event appears; assert Shutdown returned nil in under 2s.
  - `TestDrainLetsServerShutdownReturn`: build an `*http.Server` around the handler on a `net.Listen("tcp", "127.0.0.1:0")` listener (not httptest, so `Shutdown` can be called); open a subscription stream and read its first event; call `d.Shutdown(ctx)` and `srv.Shutdown(ctx)` together with a 5s ctx (use `sync.WaitGroup.Go`); assert both return nil and the whole thing takes under 2s. Without the drain `srv.Shutdown` would wait the full 5s — this is the user-visible point of the feature.
  - `TestDrainRefusesNewSubscription`: `d.Shutdown` first, then POST a subscription; assert status 503.
  - `TestDrainLeavesSingleResultAlone`: `d.Shutdown` first, then POST a query (`{ ping }` or whatever the fixture has); assert 200 with `next` then `complete` — single-result requests are the server's to drain, not the drain's.
  - `gqlecho`: serve `gqlecho.SSE(exec, gqlsse.WithDrain(d))` the way the existing gqlecho SSE test does; open a subscription, `d.Shutdown`, assert the stream ends with no `complete`.

- [ ] **Step 2: See them fail** — build failure on `gqlsse.WithDrain`.

- [ ] **Step 3: Implement** in `handler.go`: import drain; field `drain *drain.Drain`; option

```go
// WithDrain registers every subscription stream with d, so d.Shutdown ends
// them without a complete event, which lets the client reconnect. Queries and
// mutations are ordinary requests that the server's own Shutdown waits for.
// Once d is draining, new subscriptions are refused with 503.
func WithDrain(d *drain.Drain) Option { return func(h *Handler) { h.drain = d } }
```

In `subscribe`, replace `ctx := r.Context()` with

```go
	ctx, leave, ok := h.drain.Enter(r.Context())
	if !ok {
		h.writeError(w, http.StatusServiceUnavailable, "The server is shutting down.")
		return
	}
	defer leave()
```

and add to the streaming `select`:

```go
		case <-h.drain.Closing():
			// No complete: it would tell the client the subscription ended
			// for good, where ending the response makes it reconnect.
			return
```

- [ ] **Step 4: Green** — `go test -race -count=1 ./transport/gqlsse/ ./transport/gqlecho/ ./transport/`.

- [ ] **Step 5: Breaks:** remove the `Closing` case → `TestDrainEndsStreamWithoutComplete` and `TestDrainLetsServerShutdownReturn` fail; remove the `!ok` branch → `TestDrainRefusesNewSubscription` fails; write `complete` before returning in the `Closing` case → `TestDrainEndsStreamWithoutComplete` fails.

- [ ] **Step 6: Commit** — `feat: drain gqlsse subscription streams through WithDrain`.

---

### Task 5: `gqlfiber.WS` and `gqlfiber.SSE`

**Files:** Modify `transport/gqlfiber/gqlfiber.go` (config + option), `ws.go`, `sse.go`; tests in `transport/gqlfiber/` (new `drain_test.go`).

**Interfaces:** Consumes `drain.Drain`, `gqlwsproto.Config.Closing`, `gqlwsproto.StatusGoingAway`. Produces `gqlfiber.WithDrain(d *drain.Drain) Option`, `gqlfiber.StatusGoingAway`.

- [ ] **Step 1: Tests** (use `startFiber`, `newTestExecutor`, `countdownExecutor`, `dialWS`/`dialWSAt`, `wsClient.recvClose`, and the SSE helpers already in `sse_test.go`):
  - `TestWSDrainClosesWithGoingAway`: WS app with `WithDrain(d)`; `init`; open a subscription that stays open (use `idleWSSource`/`newIdleWSSource` and `waitRegistered` from `ws_test.go`); `d.Shutdown` with 5s ctx in a goroutine; assert `recvClose() == coderws.StatusGoingAway` and Shutdown nil in under 2s.
  - `TestWSDrainRefusesUpgrade`: `d.Shutdown` first; dial; assert the dial fails with HTTP 503.
  - `TestSSEDrainEndsStreamWithoutComplete`: as Task 4's first test, through Fiber with `WithDrain(d)` and a keep-alive set so the handler does not warn.
  - `TestSSEDrainRefusesNewSubscription`: 503 after drain.

- [ ] **Step 2: See them fail** — build failure on `WithDrain`.

- [ ] **Step 3: Implement.**

`gqlfiber.go`: add `drain *drain.Drain` to `config`; option

```go
// WithDrain registers WebSocket connections and SSE subscription streams with
// d, so d.Shutdown winds them down as gqlws.WithDrain and gqlsse.WithDrain do.
// It has no effect on the plain HTTP handler, which Fiber's own Shutdown
// already waits for.
func WithDrain(d *drain.Drain) Option { return func(c *config) { c.drain = d } }
```

`ws.go`: add `StatusGoingAway = gqlwsproto.StatusGoingAway` to the status block (keep the block a mirror of `gqlws`). In the `websocket.New` callback, right after `defer func() { _ = conn.Close() }()`:

```go
		// Checked again here because the check below the upgrade can race a
		// drain that starts in between; the socket is open by now, so the
		// refusal is a close rather than a status.
		ctx, leave, ok := cfg.drain.Enter(context.Background())
		if !ok {
			_ = sock.Close(StatusGoingAway, "Going away")
			return
		}
		defer leave()
```

pass `ctx` instead of `context.Background()` to `gqlwsproto.Serve` and add `Closing: cfg.drain.Closing(),`. In the returned `fiber.Handler`, after the upgrade check:

```go
		select {
		case <-cfg.drain.Closing():
			return fiber.NewError(fiber.StatusServiceUnavailable, "The server is shutting down.")
		default:
		}
```

`sse.go`, `subscribe`: replace `ctx, cancel := requestContext(c)` with

```go
	reqCtx, cancel := requestContext(c)
	ctx, leave, ok := h.drain.Enter(reqCtx)
	if !ok {
		cancel()
		h.writeError(c, http.StatusServiceUnavailable, "The server is shutting down.")
		return nil
	}
```

In the `Subscribe` error branch add `leave()` before `cancel()`. In the stream writer's cleanup defer, call `leave()` after the `for resp := range events` drain loop (last statement), so the drain waits until the stream's own cleanup is done. In `stream`, add to the select:

```go
		case <-h.drain.Closing():
			// No complete event, as in gqlsse: the client should reconnect.
			return
```

- [ ] **Step 4: Green** — `go test -race -count=1 ./transport/gqlfiber/`.

- [ ] **Step 5: Breaks:** remove `Closing: cfg.drain.Closing(),` → WS drain test fails; remove the pre-upgrade `select` → `TestWSDrainRefusesUpgrade` fails (the in-callback Enter closes 1001 instead of 503 — confirm that is what the failing assertion shows); remove the SSE `Closing` case → SSE drain test fails; remove the SSE `!ok` branch → refusal test fails.

- [ ] **Step 6: Commit** — `feat: drain gqlfiber WebSocket and SSE connections through WithDrain`.

---

### Task 6: Examples and documentation

Controller task.

- [ ] Wire `drain.New()` into `examples/quickstart`, `examples/blog/cmd/server`, `examples/echo`, `examples/fiber` wherever they serve SSE or WebSocket, running `d.Shutdown` alongside the server's shutdown with the same context (`sync.WaitGroup.Go`).
- [ ] `CLAUDE.md` Transports section: a paragraph on the drain — why the servers do not drain these connections (quote the net/http doc), the per-transport behaviour, why queries finish and subscriptions do not, why no `complete`, the hard cut at the deadline, `gqlwsproto.watch` closing the socket because `gqlfiber`'s Read ignores its context, and that `gqlhttp` needs nothing. Update the Status line.
- [ ] `README.md`: mention `drain` with the wiring snippet where transports are described.
- [ ] `sh scripts/gate.sh` (all modules), then commit `docs: wire the drain into the examples and document it`.
