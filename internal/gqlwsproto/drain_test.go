package gqlwsproto

import (
	"context"
	"errors"
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
func serveDraining(t *testing.T, ctx context.Context, sock Socket, exec *graphql.Executor) (closing chan struct{}, done chan struct{}) {
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

// TestCancelledContextClosesSocket: a cancel from outside, with no drain in
// progress, ends the connection. A driver whose Read ignores its context
// (gqlfiber) can only be unblocked by closing the socket, so Serve must do
// that.
func TestCancelledContextClosesSocket(t *testing.T) {
	_, exec := newDrainExecutor(t)
	fake := newFakeSocket()
	sock := deafSocket{fake}
	fake.in <- []byte(`{"type":"connection_init"}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(ctx, sock, Config{Exec: exec, InitTimeout: 5 * time.Second, MaxSubs: 10})
	}()
	waitFor(t, "the ack", func() bool { return slices.Contains(fake.types(), "connection_ack") })
	cancel()
	waitDone(t, done)
	if got := fake.closeCode(); got != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, StatusGoingAway)
	}
}

// deafSocket is a driver whose Read ignores its context, as gqlfiber's does:
// only closing the socket ends a parked read.
type deafSocket struct{ *fakeSocket }

func (d deafSocket) Read(context.Context) ([]byte, error) {
	b, ok := <-d.in
	if !ok {
		return nil, context.Canceled
	}
	return b, nil
}

// TestDrainGivingUpClosesSocket: a drain waiting on an operation that ignores
// cancellation gives up by cancelling the connection context. The watcher is
// by then parked in the drain rather than watching the context, and must still
// close the socket: with a deaf Read nothing else ends the read loop, and with
// an attentive one Serve's own wait blocks on the same operation.
func TestDrainGivingUpClosesSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		deaf bool
	}{{"deaf read", true}, {"attentive read", false}} {
		t.Run(tc.name, func(t *testing.T) {
			src, exec := newDrainExecutor(t)
			fake := newFakeSocket()
			var sock Socket = fake
			if tc.deaf {
				sock = deafSocket{fake}
			}
			fake.in <- []byte(`{"type":"connection_init"}`)
			fake.in <- []byte(`{"id":"q","type":"subscribe","payload":{"query":"{ block }"}}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			closing, done := serveDraining(t, ctx, sock, exec)
			defer func() {
				close(src.release)
				waitDone(t, done)
			}()

			<-src.entered
			close(closing)
			time.Sleep(20 * time.Millisecond) // let the watcher settle into the drain
			cancel()
			waitFor(t, "the close after the drain gave up", func() bool { return fake.closeCode() == StatusGoingAway })
		})
	}
}

// TestCancelledConnectContextClosesSocket: a context OnConnect returned can
// end on its own, as a token expiring would. No read is cancellable any more,
// so without something watching that context the client would be left on an
// open socket that neither pings nor serves operations.
func TestCancelledConnectContextClosesSocket(t *testing.T) {
	_, exec := newDrainExecutor(t)
	fake := newFakeSocket()
	sock := deafSocket{fake}
	fake.in <- []byte(`{"type":"connection_init"}`)
	session, expire := context.WithCancel(context.Background())
	defer expire()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{
			Exec: exec, InitTimeout: 5 * time.Second, MaxSubs: 10,
			OnConnect: func(context.Context, []byte) (context.Context, error) {
				return session, nil
			},
		})
	}()
	waitFor(t, "the ack", func() bool { return slices.Contains(fake.types(), "connection_ack") })
	expire()
	waitDone(t, done)
	if got := fake.closeCode(); got != StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, StatusGoingAway)
	}
}

// TestServePanicReleasesWatch: net/http recovers a panicking handler, so a
// panic out of Serve (here from OnConnect) must not leave watch parked for the
// life of the process. Counting goroutines would be fragile; a watch still
// running is observable instead, because it closes the socket with 1001 when
// the caller's context is later cancelled, and one that exited cannot.
func TestServePanicReleasesWatch(t *testing.T) {
	_, exec := newDrainExecutor(t)
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		Serve(ctx, sock, Config{
			Exec: exec, InitTimeout: 5 * time.Second, MaxSubs: 10,
			OnConnect: func(context.Context, []byte) (context.Context, error) {
				panic("hook exploded")
			},
		})
	}()
	select {
	case r := <-recovered:
		if r == nil {
			t.Fatal("Serve returned without the hook's panic")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve neither returned nor panicked")
	}

	cancel()
	time.Sleep(100 * time.Millisecond) // a watch that survived the panic closes the socket in this window
	if got := sock.closeCode(); got != 0 {
		t.Fatalf("socket closed with %d after the panic: watch was still running", got)
	}
}

// brokenPongSocket fails every write after the first (the ack), without
// closing anything, as a peer that stopped reading would.
type brokenPongSocket struct {
	*fakeSocket
	writes int
}

func (b *brokenPongSocket) Write(ctx context.Context, data []byte) error {
	b.writes++ // serialized by the protocol's write lock
	if b.writes > 1 {
		return errors.New("peer stopped reading")
	}
	return b.fakeSocket.Write(ctx, data)
}

// TestServeExitDoesNotCloseGoingAway: Serve cancels the connection context on
// its way out, which is the same context the close-on-cancel watches. That
// close must be unregistered first, or every connection ending for its own
// reason would be sent a 1001 it did not earn.
func TestServeExitDoesNotCloseGoingAway(t *testing.T) {
	_, exec := newDrainExecutor(t)
	fake := newFakeSocket()
	sock := &brokenPongSocket{fakeSocket: fake}
	fake.in <- []byte(`{"type":"connection_init"}`)
	fake.in <- []byte(`{"type":"ping"}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{Exec: exec, InitTimeout: 5 * time.Second, MaxSubs: 10})
	}()
	waitDone(t, done)
	time.Sleep(20 * time.Millisecond) // context.AfterFunc runs on its own goroutine
	if got := fake.closeCode(); got != 0 {
		t.Fatalf("close code = %d after Serve ended on a failed write, want no close", got)
	}
}
