# Connection Age and Idle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** grpc-style `MaxConnectionAge` (+grace, ±10% jitter) and `MaxConnectionIdle` for WebSocket connections, and `MaxStreamAge` for SSE subscription streams, all off by default.

**Architecture:** `internal/jitter` spreads durations. `internal/gqlwsproto` reuses its drain for age (per connection, with an optional grace cut) and adds an idle timer driven by the operation count. SSE handlers add an age case to their streaming select.

**Tech Stack:** Go 1.27, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-17-connection-age-design.md`

## Global Constraints

- All new options default to zero = no limit; without them behaviour is unchanged.
- Close codes: age -> 1001 `Going away` (`StatusGoingAway`); idle -> 1000 `Idle timeout` (`StatusNormalClosure`, new).
- Refusal messages exactly: `The connection has reached its maximum age.` / `The connection was idle.`; the shutdown drain keeps `The server is shutting down.`
- Jitter is uniform within ±10% of the configured age.
- gqlwsproto invariants from CLAUDE.md stay intact: writes under the connection context; Close outside the write lock; doubly redundant release; reads never cancellable; watch closes the socket.
- Timing tests run inside `testing/synctest`; every other wait has an explicit deadline.
- `-race` on every run; every new test broken on purpose once (reverse edits only, never `git checkout -- <file>`).
- Comments explain why, English only. Commits lower-case typed, imperative, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

### Task 1: `internal/jitter` and the WebSocket protocol

**Files:** Create `internal/jitter/jitter.go`, `internal/jitter/jitter_test.go`, `internal/gqlwsproto/age_test.go`. Modify `internal/gqlwsproto/config.go`, `protocol.go`, `conn.go`.

**Produces:** `jitter.Spread(d time.Duration) time.Duration`; `Config.MaxConnectionAge`, `Config.MaxConnectionAgeGrace`, `Config.MaxConnectionIdle`; `StatusNormalClosure = 1000`.

- [ ] **Step 1: Tests.** `internal/jitter/jitter_test.go`:

```go
package jitter

import (
	"testing"
	"time"
)

func TestSpreadStaysWithinTenPercent(t *testing.T) {
	const d = time.Hour
	lo, hi := d-d/10, d+d/10
	seen := map[time.Duration]bool{}
	for range 10_000 {
		got := Spread(d)
		if got < lo || got > hi {
			t.Fatalf("Spread(%v) = %v, outside [%v, %v]", d, got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct values in 10000 samples; the jitter is not spreading", len(seen))
	}
}

func TestSpreadLeavesNonPositiveAlone(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := Spread(d); got != d {
			t.Fatalf("Spread(%v) = %v, want it unchanged", d, got)
		}
	}
}
```

`internal/gqlwsproto/age_test.go` (reuses `newFakeSocket`, `types`, `closeCode`, `newDrainExecutor` from the package's other tests):

```go
package gqlwsproto

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func serveWith(sock Socket, cfg Config) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, cfg)
	}()
	return done
}

func isDone(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func wrote(sock *fakeSocket, s string) bool {
	sock.mu.Lock()
	defer sock.mu.Unlock()
	for _, b := range sock.out {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

const (
	initMsg      = `{"type":"connection_init"}`
	subscribeSub = `{"id":"1","type":"subscribe","payload":{"query":"subscription { tick }"}}`
	subscribeQry = `{"id":"q","type":"subscribe","payload":{"query":"{ block }"}}`
)

// TestMaxConnectionAgeDrains: an age limit ends a connection within ±10% of
// the age, as a drain — the subscription gets no complete, which would tell
// the client not to resubscribe once it reconnects.
func TestMaxConnectionAgeDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeSub)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionAge: time.Hour})

		time.Sleep(53 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d before 90%% of its age", code)
		}
		time.Sleep(14 * time.Minute)
		synctest.Wait()
		if !isDone(done) {
			t.Fatal("still open past 110% of its age")
		}
		if code := sock.closeCode(); code != StatusGoingAway {
			t.Fatalf("close code = %d, want %d", code, StatusGoingAway)
		}
		if slices.Contains(sock.types(), "complete") {
			t.Fatalf("age drain sent complete: %v", sock.types())
		}
	})
}

// TestMaxConnectionAgeLetsQueryFinish: inside its grace a query in flight
// finishes and its client learns the result before the close.
func TestMaxConnectionAgeLetsQueryFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeQry)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10,
			MaxConnectionAge: time.Hour, MaxConnectionAgeGrace: time.Hour})

		<-src.entered
		time.Sleep(67 * time.Minute)
		synctest.Wait()
		if isDone(done) || sock.closeCode() != 0 {
			t.Fatal("closed while a query was running inside its grace")
		}
		close(src.release)
		<-done
		got := sock.types()
		if n := len(got); n < 2 || got[n-2] != "next" || got[n-1] != "complete" {
			t.Fatalf("message types = %v, want the query's next and complete last", got)
		}
		if code := sock.closeCode(); code != StatusGoingAway {
			t.Fatalf("close code = %d, want %d", code, StatusGoingAway)
		}
	})
}

// TestMaxConnectionAgeGraceCuts: past its grace the connection closes even
// with an operation still running.
func TestMaxConnectionAgeGraceCuts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeQry)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10,
			MaxConnectionAge: time.Hour, MaxConnectionAgeGrace: 10 * time.Minute})

		<-src.entered
		time.Sleep(77 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusGoingAway {
			t.Fatalf("close code = %d past age plus grace, want %d", code, StatusGoingAway)
		}
		close(src.release)
		<-done
	})
}

// TestMaxConnectionAgeRefusesNewOperations: once the age drain has begun a
// new operation is refused with the age's own message.
func TestMaxConnectionAgeRefusesNewOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeQry)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionAge: time.Hour})

		<-src.entered
		time.Sleep(67 * time.Minute)
		synctest.Wait()
		sock.in <- []byte(subscribeSub)
		synctest.Wait()
		if !wrote(sock, "The connection has reached its maximum age.") {
			t.Fatalf("no age refusal written: %v", sock.types())
		}
		close(src.release)
		<-done
	})
}

// TestMaxConnectionIdleCloses: with nothing in flight the connection closes
// normally after the idle period, not before.
func TestMaxConnectionIdleCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionIdle: 10 * time.Minute})

		time.Sleep(9 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d before the idle period", code)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d after the idle period, want %d", code, StatusNormalClosure)
		}
		<-done
	})
}

// TestMaxConnectionIdleWaitsForOperations: an open subscription is not idle,
// and the idle period starts again when the last operation ends.
func TestMaxConnectionIdleWaitsForOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeSub)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10, MaxConnectionIdle: 10 * time.Minute})

		time.Sleep(30 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d while a subscription was open", code)
		}
		sock.in <- []byte(`{"id":"1","type":"complete"}`)
		time.Sleep(9 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d less than the idle period after the last operation ended", code)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d, want %d", code, StatusNormalClosure)
		}
		<-done
	})
}

// TestNoLimitsKeepsConnectionOpen guards the defaults.
func TestNoLimitsKeepsConnectionOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exec := newDrainExecutor(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{Exec: exec, InitTimeout: time.Minute, MaxSubs: 10})

		time.Sleep(100 * time.Hour)
		synctest.Wait()
		if code := sock.closeCode(); code != 0 {
			t.Fatalf("closed with %d with no limits configured", code)
		}
		_ = sock.Close(1000, "test done")
		<-done
	})
}
```

- [ ] **Step 2: See them fail** — build failure on `Spread`, `MaxConnectionAge`, `StatusNormalClosure`.

- [ ] **Step 3: Implement.**

`internal/jitter/jitter.go`:

```go
// Package jitter spreads durations so that connections opened together do
// not all expire together and reconnect in one storm.
package jitter

import (
	"math/rand/v2"
	"time"
)

// Spread returns d moved uniformly by up to 10% either way, the spread grpc
// applies to MaxConnectionAge. Zero and negative durations are returned
// unchanged.
func Spread(d time.Duration) time.Duration {
	span := int64(d) / 5
	if d <= 0 || span == 0 {
		return d
	}
	return d - time.Duration(span/2) + time.Duration(rand.Int64N(span+1))
}
```

`protocol.go`: add as the first entry of the status block

```go
	// StatusNormalClosure is RFC 6455's 1000, sent when an idle connection is
	// closed: nothing was in flight, so a client reconnects only when it next
	// needs to.
	StatusNormalClosure            = 1000
```

`config.go`, in `Config` after `Closing`:

```go
	// MaxConnectionAge, when positive, drains a connection once it has been
	// open this long, give or take 10% so that connections opened together
	// do not drain together: new operations are refused, subscriptions end
	// without complete, queries and mutations finish, and the connection
	// closes with StatusGoingAway. It lets a load balancer spread long-lived
	// connections again. Optional.
	MaxConnectionAge time.Duration

	// MaxConnectionAgeGrace, when positive, bounds how long an age drain
	// waits for operations before closing anyway. Zero waits for them.
	MaxConnectionAgeGrace time.Duration

	// MaxConnectionIdle, when positive, closes a connection with
	// StatusNormalClosure once it has had no operation in flight for this
	// long, counted from the handshake or from the last operation ending.
	// Subscriptions count as in flight; pings do not. Optional.
	MaxConnectionIdle time.Duration
```

`conn.go`:

1. Import `"github.com/syssam/graphql-go/internal/jitter"`.
2. `conn` struct, after `draining bool`:

```go
	// drainReason is what a refused operation is told; set with draining.
	drainReason string

	// idle closes the connection once it has had no operation in flight for
	// MaxConnectionIdle; nil without that limit. Stopped and reset under mu.
	idle *time.Timer
```

3. `Serve`: before `served, watched := ...`, add

```go
	var aged <-chan time.Time
	if cfg.MaxConnectionAge > 0 {
		t := time.NewTimer(jitter.Spread(cfg.MaxConnectionAge))
		defer t.Stop()
		aged = t.C
	}
```

and call `c.watch(parent, served, aged)`.

4. `watch`: new signature `func (c *conn) watch(parent context.Context, served <-chan struct{}, aged <-chan time.Time)`; the `Closing` case becomes `c.drain(parent, served, "The server is shutting down.", nil)`; add

```go
	case <-aged:
		var grace <-chan time.Time
		if g := c.cfg.MaxConnectionAgeGrace; g > 0 {
			t := time.NewTimer(g)
			defer t.Stop()
			grace = t.C
		}
		c.drain(parent, served, "The connection has reached its maximum age.", grace)
```

5. `drain`: new signature `func (c *conn) drain(parent context.Context, served <-chan struct{}, reason string, grace <-chan time.Time)`; set `c.drainReason = reason` beside `c.draining = true`; add to the wait's select

```go
	case <-grace:
		c.close(StatusGoingAway, "Going away")
		return
```

(A nil `grace` channel never fires.)

6. `subscribe`: the refusal becomes

```go
	if c.draining || closed(c.cfg.Closing) {
		reason := c.drainReason
		if !c.draining {
			reason = "The server is shutting down."
		}
		c.mu.Unlock()
		cancel()
		return c.writeError(msg.ID, graphql.Errorf("%s", reason)) == nil
	}
```

and after `c.subs[msg.ID] = cancel`, add

```go
	if c.idle != nil && len(c.subs) == 1 {
		c.idle.Stop()
	}
```

7. `handshake`, right after the `c.stopOnDone = context.AfterFunc(...)` statement:

```go
			if d := c.cfg.MaxConnectionIdle; d > 0 {
				c.idle = time.AfterFunc(d, c.closeIfIdle)
			}
```

8. `stop` and `forget`: under the lock, only reset when an id was actually removed:

```go
func (c *conn) stop(id string) {
	c.mu.Lock()
	cancel, ok := c.subs[id]
	delete(c.subs, id)
	if ok {
		c.resetIdleLocked()
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *conn) forget(id string) {
	c.mu.Lock()
	if _, ok := c.subs[id]; ok {
		delete(c.subs, id)
		c.resetIdleLocked()
	}
	c.mu.Unlock()
}

// resetIdleLocked starts the idle period again when the last operation has
// ended. A complete for an id that was never running changes nothing, so it
// cannot keep an idle connection alive.
func (c *conn) resetIdleLocked() {
	if c.idle != nil && len(c.subs) == 0 && !c.draining {
		c.idle.Reset(c.cfg.MaxConnectionIdle)
	}
}

// closeIfIdle runs when the idle period ends. An operation may have started
// since the timer fired, so it checks again under the lock, and marks the
// connection draining so a subscribe racing the close is refused.
func (c *conn) closeIfIdle() {
	c.mu.Lock()
	if len(c.subs) > 0 || c.draining {
		c.mu.Unlock()
		return
	}
	c.draining = true
	c.drainReason = "The connection was idle."
	c.mu.Unlock()
	c.close(StatusNormalClosure, "Idle timeout")
}
```

9. `serve`'s defer: before `c.stopOnDone()`, add

```go
		// Teardown deletes every operation; marking draining first keeps
		// those deletions from re-arming the idle timer.
		c.mu.Lock()
		c.draining = true
		if c.idle != nil {
			c.idle.Stop()
		}
		c.mu.Unlock()
```

- [ ] **Step 4: Green** — `go vet ./internal/... && go test -race -count=1 ./internal/jitter/ ./internal/gqlwsproto/ ./transport/gqlws/ ./transport/gqlfiber/`, then `-count=20` on the new tests.

- [ ] **Step 5: Breaks** (each must fail its test):
  1. `watch`: delete the `case <-aged:` block -> `TestMaxConnectionAgeDrains`.
  2. `drain`: delete the `case <-grace:` block -> `TestMaxConnectionAgeGraceCuts`.
  3. `subscribe`: always use `"The server is shutting down."` -> `TestMaxConnectionAgeRefusesNewOperations`.
  4. `subscribe`: delete the `c.idle.Stop()` block -> `TestMaxConnectionIdleWaitsForOperations`.
  5. `resetIdleLocked`: make it empty -> `TestMaxConnectionIdleWaitsForOperations`.
  6. `jitter.Spread`: return `d` -> `TestSpreadStaysWithinTenPercent`.

- [ ] **Step 6: Commit** — `feat: add connection age and idle limits to graphql-transport-ws`.

---

### Task 2: WebSocket driver options (`gqlws`, `gqlfiber.WS`)

**Files:** `transport/gqlws/handler.go`, `transport/gqlfiber/gqlfiber.go`, `transport/gqlfiber/ws.go`; tests in `transport/gqlws/conn_test.go` and `transport/gqlfiber/ws_test.go` (or new `age_test.go` files).

**Consumes:** `gqlwsproto.Config.MaxConnectionAge/MaxConnectionAgeGrace/MaxConnectionIdle`, `gqlwsproto.StatusNormalClosure`.

- [ ] Options, with doc comments that say what the protocol does (paraphrase the Config docs; mention ±10% jitter, 1001 for age, 1000 for idle, zero means no limit):

```go
// gqlws
func WithMaxConnectionAge(age, grace time.Duration) Option {
	return func(h *Handler) { h.maxAge, h.maxAgeGrace = age, grace }
}
func WithMaxConnectionIdle(d time.Duration) Option { return func(h *Handler) { h.maxIdle = d } }

// gqlfiber (config fields maxAge, maxAgeGrace, maxIdle; WS only)
func WithMaxConnectionAge(age, grace time.Duration) Option {
	return func(c *config) { c.maxAge, c.maxAgeGrace = age, grace }
}
func WithMaxConnectionIdle(d time.Duration) Option { return func(c *config) { c.maxIdle = d } }
```

Pass them into the `gqlwsproto.Config` literal in both drivers. Add `StatusNormalClosure = gqlwsproto.StatusNormalClosure` to both packages' status blocks (keep them mirrors).

- [ ] Tests over real connections (reuse existing dial helpers). These cannot use synctest (real sockets and servers), so use small real durations with explicit deadlines:
  - age: `WithMaxConnectionAge(100*time.Millisecond, 0)`, init, open a subscription, require close status 1001 within 2s and not within 50ms.
  - idle: `WithMaxConnectionIdle(100*time.Millisecond)`, init only, require close status 1000 within 2s.
  Break: drop the option fields from the Config literal -> each fails.

- [ ] Commit `feat: expose connection age and idle limits on gqlws and gqlfiber`.

---

### Task 3: SSE stream age (`gqlsse`, `gqlfiber.SSE`)

**Files:** `transport/gqlsse/handler.go`, `transport/gqlfiber/gqlfiber.go` (option), `transport/gqlfiber/sse.go`; tests beside them.

**Consumes:** `jitter.Spread`.

- [ ] Options: `gqlsse.WithMaxStreamAge(d time.Duration) Option` (field `maxStreamAge`), `gqlfiber.WithMaxStreamAge(d time.Duration) Option` (config field `maxStreamAge`, SSE only). Doc: a subscription stream ends after d give or take 10%, without a complete event, so the client reconnects; single-result requests are unaffected; zero means no limit.
- [ ] In `gqlsse.Handler.subscribe` before the loop, and in `gqlfiber`'s `sseHandler.stream` before its loop:

```go
	var aged <-chan time.Time
	if h.maxStreamAge > 0 {
		t := time.NewTimer(jitter.Spread(h.maxStreamAge))
		defer t.Stop()
		aged = t.C
	}
```

and a select case:

```go
		case <-aged:
			// No complete, as with a drain: the client should reconnect.
			return
```

- [ ] Tests (reuse each package's SSE helpers): `WithMaxStreamAge(100*time.Millisecond)`; open a subscription stream; require the body to end within 2s with no `complete` event; and a single-result query on the same handler still gets `next` then `complete`. Break: remove the `aged` case -> the stream test fails (bounded read).
- [ ] Commit `feat: bound SSE subscription streams with WithMaxStreamAge`.

---

### Task 4: Documentation (controller)

- [ ] `CLAUDE.md` Transports: a paragraph — grpc parity (age + grace + jitter, idle), why age reuses the drain, the idle definition (subscriptions count; pings and unknown completes do not), close codes, SSE stream age, and that the timing tests run in synctest. Update Status.
- [ ] `README.md`: one sentence and the options where transports are described.
- [ ] `sh scripts/gate.sh`; commit `docs: document connection age and idle limits`.
