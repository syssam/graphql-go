package gqlwsproto

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// closeIfIdle re-checks idleDeadline after taking mu, and that re-check was
// carried on reasoning alone: "that interleaving needs the callback parked on
// mu across a whole operation, which no test drives deterministically."
//
// It does not need real timing. The interleaving is entirely about what the
// callback sees once it gets the lock, so a test that holds mu itself, starts
// the callback, re-arms the deadline the way forget does, and then releases
// the lock drives exactly the case the comment describes -- with no sleep, no
// timer and nothing to flake. The goroutine either blocks on Lock and sees the
// new deadline afterwards, or has not reached Lock yet and sees it when it
// does; both are the case under test.
func idleConn(sock Socket) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{
		cfg:    Config{Logger: slog.New(slog.DiscardHandler)},
		sock:   sock,
		ctx:    ctx,
		cancel: cancel,
		subs:   map[string]*operation{},
	}
}

// The base case first, so the test is known to reach the close it is about to
// assert the absence of. Without this, a closeIfIdle that returned
// unconditionally would pass the real assertion below.
func TestCloseIfIdleClosesAnIdleConnection(t *testing.T) {
	sock := newFakeSocket()
	c := idleConn(sock)
	// Zero time is in the past, so the connection is idle and due.
	c.closeIfIdle()
	if got := sock.closeCode(); got != StatusNormalClosure {
		t.Fatalf("close code = %d, want %d; the idle close did not happen", got, StatusNormalClosure)
	}
	if !c.draining {
		t.Error("draining was not set")
	}
}

func TestCloseIfIdleDoesNotCutOffAnOperationThatReArmedTheDeadline(t *testing.T) {
	sock := newFakeSocket()
	c := idleConn(sock)

	done := make(chan struct{})
	c.mu.Lock()
	go func() {
		defer close(done)
		c.closeIfIdle()
	}()
	// What forget does when an operation ends: the connection has not been
	// idle for the full period any more, so the timer that already fired is
	// stale.
	c.idleDeadline = time.Now().Add(time.Hour)
	c.mu.Unlock()
	<-done

	if got := sock.closeCode(); got != 0 {
		t.Errorf("close code = %d; a stale idle timer closed a connection whose "+
			"deadline had been re-armed, cutting off the operation's complete", got)
	}
	if c.draining {
		t.Error("draining was set, so the next operation would be refused")
	}
}

// The other two conditions in the same re-check, for the same reason: they are
// read after the lock is taken, so what they guard is an operation that
// started while the callback waited.
func TestCloseIfIdleDoesNotCloseWithASubscriptionOrWhileDraining(t *testing.T) {
	t.Run("subscription started", func(t *testing.T) {
		sock := newFakeSocket()
		c := idleConn(sock)
		done := make(chan struct{})
		c.mu.Lock()
		go func() { defer close(done); c.closeIfIdle() }()
		_, cancel := context.WithCancel(context.Background())
		c.subs["1"] = &operation{cancel: cancel}
		c.mu.Unlock()
		<-done
		cancel()
		if got := sock.closeCode(); got != 0 {
			t.Errorf("close code = %d; a live subscription was cut off", got)
		}
	})
	t.Run("already draining", func(t *testing.T) {
		sock := newFakeSocket()
		c := idleConn(sock)
		done := make(chan struct{})
		c.mu.Lock()
		go func() { defer close(done); c.closeIfIdle() }()
		c.draining = true
		c.drainReason = "The server is shutting down."
		c.mu.Unlock()
		<-done
		if c.drainReason != "The server is shutting down." {
			t.Errorf("drainReason = %q; the idle close overwrote the reason a "+
				"drain already set, so a client is told the wrong thing", c.drainReason)
		}
	})
}

// cancelAll's loop is the other half of a deliberately redundant pair: every
// subscription's context is already a child of c.ctx, so c.cancel() alone
// frees them, and breaking just the loop leaves every test in the repository
// green. But the loop has a contract of its own that does not depend on the
// context tree at all -- every cancel registered in c.subs is called, and
// c.subs is emptied -- and that is testable directly by registering a cancel
// the connection context cannot reach.
//
// Without this, the only thing standing behind the loop was the reasoning in
// its own comment.
func TestCancelAllCallsEveryRegisteredCancel(t *testing.T) {
	sock := newFakeSocket()
	c := idleConn(sock)

	// Deliberately not derived from c.ctx: if these were children, c.cancel()
	// would free them and the loop could be missing without the test noticing,
	// which is exactly the blind spot being closed.
	called := map[string]bool{}
	for _, id := range []string{"1", "2", "3"} {
		c.subs[id] = &operation{cancel: func() { called[id] = true }}
	}

	c.cancelAll()

	for _, id := range []string{"1", "2", "3"} {
		if !called[id] {
			t.Errorf("subscription %s was never cancelled; it is freed only by the "+
				"connection context, so a change to that derivation leaks it", id)
		}
	}
	if len(c.subs) != 0 {
		t.Errorf("subs still holds %d entries after cancelAll", len(c.subs))
	}
	// And the connection context, which is the half that already had cover.
	if c.ctx.Err() == nil {
		t.Error("the connection context was not cancelled")
	}
}
