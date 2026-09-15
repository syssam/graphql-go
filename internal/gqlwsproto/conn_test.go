package gqlwsproto

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// fakeSocket is an in-memory Socket. It records every write so a test can
// assert the message sequence without a real connection.
type fakeSocket struct {
	in chan []byte

	mu     sync.Mutex
	out    [][]byte
	closed bool
	code   int
}

func newFakeSocket() *fakeSocket { return &fakeSocket{in: make(chan []byte, 16)} }

func (f *fakeSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case b, ok := <-f.in:
		if !ok {
			return nil, context.Canceled
		}
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeSocket) Write(ctx context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, append([]byte(nil), data...))
	return nil
}

func (f *fakeSocket) Close(code int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed, f.code = true, code
		close(f.in)
	}
	return nil
}

func (f *fakeSocket) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.out))
	for _, b := range f.out {
		var m struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(b, &m)
		out = append(out, m.Type)
	}
	return out
}

func (f *fakeSocket) closeCode() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code
}

func TestServeAcknowledgesInit(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(`{"type":"connection_init"}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{InitTimeout: time.Second, MaxSubs: 10})
	}()

	// Closing the socket ends the read loop after the ack.
	time.Sleep(50 * time.Millisecond)
	_ = sock.Close(1000, "done")
	<-done

	got := sock.types()
	if len(got) == 0 || got[0] != "connection_ack" {
		t.Fatalf("message types = %v, want connection_ack first", got)
	}
}

// The init timeout must close the connection with 4408 rather than abort the
// read, or the client sees an abnormal closure and cannot tell why.
func TestServeInitTimeoutCloses4408(t *testing.T) {
	sock := newFakeSocket()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{InitTimeout: 20 * time.Millisecond, MaxSubs: 10})
	}()
	<-done

	if got := sock.closeCode(); got != StatusInitTimeout {
		t.Fatalf("close code = %d, want %d", got, StatusInitTimeout)
	}
}

// A message before connection_init is unauthorized.
func TestServeSubscribeBeforeInitIsUnauthorized(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{a}"}}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(context.Background(), sock, Config{InitTimeout: time.Second, MaxSubs: 10})
	}()
	<-done

	if got := sock.closeCode(); got != StatusUnauthorized {
		t.Fatalf("close code = %d, want %d", got, StatusUnauthorized)
	}
}
