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

// TestActiveCountsEnteredConnections: Active is what a metric reports as the
// number of long-lived connections open right now.
func TestActiveCountsEnteredConnections(t *testing.T) {
	d := drain.New()
	_, leaveA, _ := d.Enter(context.Background())
	_, leaveB, _ := d.Enter(context.Background())
	if got := d.Active(); got != 2 {
		t.Fatalf("Active = %d with two entered, want 2", got)
	}
	leaveA()
	leaveA() // a repeated leave must not count twice
	if got := d.Active(); got != 1 {
		t.Fatalf("Active = %d after one left, want 1", got)
	}
	leaveB()
	_ = d.Shutdown(context.Background())
	if _, _, ok := d.Enter(context.Background()); ok {
		t.Fatal("Enter admitted after Shutdown")
	}
	if got := d.Active(); got != 0 {
		t.Fatalf("Active = %d after all left and a refused Enter, want 0", got)
	}
	var nilDrain *drain.Drain
	if got := nilDrain.Active(); got != 0 {
		t.Fatalf("nil Drain Active = %d, want 0", got)
	}
}
