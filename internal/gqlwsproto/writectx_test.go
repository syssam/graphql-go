package gqlwsproto

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ctxSocket records the context of every write, and reports a close without
// ending the read loop, so a test can look at those contexts while the
// connection is still up.
type ctxSocket struct {
	*fakeSocket
	mu     sync.Mutex
	ctxs   []context.Context
	closes chan int
}

func (s *ctxSocket) Write(ctx context.Context, data []byte) error {
	s.mu.Lock()
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	return s.fakeSocket.Write(ctx, data)
}

func (s *ctxSocket) Close(code int, _ string) error {
	s.closes <- code
	return nil
}

// A hook's context ends when the token it decoded expires, and the
// connection then closes 1001 so the client re-authenticates. coder/websocket
// tears a connection down without a close frame when a write's context is
// cancelled mid-write, so a write under the hook's context -- the ack, a
// result, a pong -- racing the expiry dropped the socket instead: the client
// read an abnormal closure, not 1001 (TestCancelledConnectContextCloses in
// transport/gqlws, under load). Writes use the connection's own lifetime.
func TestHookExpiryCancelsNoWrite(t *testing.T) {
	sock := &ctxSocket{fakeSocket: newFakeSocket(), closes: make(chan int, 4)}
	session, expire := context.WithCancel(context.Background())
	defer expire()
	served := make(chan struct{})
	go func() {
		defer close(served)
		Serve(context.Background(), sock, Config{
			InitTimeout: time.Second,
			MaxSubs:     10,
			OnConnect: func(ctx context.Context, _ []byte) (context.Context, error) {
				ctx, cancel := context.WithCancel(ctx)
				context.AfterFunc(session, cancel)
				return ctx, nil
			},
		})
	}()
	sock.in <- []byte(`{"type":"connection_init"}`)
	sock.in <- []byte(`{"type":"ping"}`)
	deadline := time.Now().Add(2 * time.Second)
	for len(sock.types()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no ack and pong: %v", sock.types())
		}
		time.Sleep(time.Millisecond)
	}

	expire()
	select {
	case code := <-sock.closes:
		if code != StatusGoingAway {
			t.Fatalf("close code %d, want %d", code, StatusGoingAway)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection did not close after its hook context ended")
	}
	sock.mu.Lock()
	for i, ctx := range sock.ctxs {
		if ctx.Err() != nil {
			t.Errorf("write %d (%s) ran under a context the hook's expiry cancelled", i, sock.types()[i])
		}
	}
	sock.mu.Unlock()

	close(sock.in)
	<-served
}
