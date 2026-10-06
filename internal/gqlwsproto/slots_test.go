package gqlwsproto

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// deafFixture is a query that does not look at its context, so an operation
// the client has completed goes on running until the gate opens.
type deafFixture struct {
	gate          chan struct{}
	once          sync.Once
	running, peak atomic.Int64
	exec          *graphql.Executor
}

func newDeafFixture(t *testing.T) *deafFixture {
	t.Helper()
	f := &deafFixture{gate: make(chan struct{})}
	s, err := graphql.NewSchema(graphql.SDL(`type Query { ping: String! slow: String! }`),
		graphql.Query(
			graphql.Field("ping", func(graphql.Root) string { return "pong" }),
			graphql.Resolve("slow", func(context.Context, graphql.Root) (string, error) {
				if n := f.running.Add(1); n > f.peak.Load() {
					f.peak.Store(n)
				}
				defer f.running.Add(-1)
				<-f.gate
				return "stale", nil
			}),
		))
	if err != nil {
		t.Fatal(err)
	}
	f.exec = graphql.NewExecutor(s)
	return f
}

// open lets every held resolver return. It is safe to call again, so a test
// can defer it against failing before it got there.
func (f *deafFixture) open() { f.once.Do(func() { close(f.gate) }) }

// MaxSubs counted the ids a connection held, and a complete gives the id back
// at once while the operation it named runs on until its resolvers return. A
// client alternating subscribe and complete therefore ran as many operations
// at a time as it liked on a connection capped at two.
func TestACompletedOperationKeepsItsSlotUntilItStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeafFixture(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		done := serveWith(sock, Config{Exec: f.exec, InitTimeout: time.Minute, MaxSubs: 2})
		defer endServe(sock, done)
		defer f.open()
		synctest.Wait()

		for i := range 10 {
			sock.in <- []byte(fmt.Sprintf(`{"id":"%d","type":"subscribe","payload":{"query":"{ slow }"}}`, i))
			synctest.Wait()
			sock.in <- []byte(fmt.Sprintf(`{"id":"%d","type":"complete"}`, i))
			synctest.Wait()
		}
		if got := f.peak.Load(); got != 2 {
			t.Fatalf("%d operations ran at once on a connection capped at 2", got)
		}
		// None of them was refused: a completed id is free, and what waits for
		// a slot is the operation, not the client.
		for _, m := range sock.frames() {
			if strings.Contains(m, `"type":"error"`) {
				t.Fatalf("a subscribe was refused: %s", m)
			}
		}

		f.open()
		synctest.Wait()
	})
}

// The protocol lets a client reuse an id the moment it has completed it. With
// the slot held by the operation it completed, the new one waits for it rather
// than being refused, and is then served under the id as usual.
func TestAnIDReusedAtTheCapWaitsForItsSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeafFixture(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{ slow }"}}`)
		done := serveWith(sock, Config{Exec: f.exec, InitTimeout: time.Minute, MaxSubs: 1})
		defer endServe(sock, done)
		defer f.open()
		synctest.Wait()

		sock.in <- []byte(completeOne)
		sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{ ping }"}}`)
		synctest.Wait()
		if got := sock.frames(); len(got) != 1 {
			t.Fatalf("frames = %q: the second operation ran, or was refused, while the first held the slot", got)
		}

		f.open()
		synctest.Wait()
		got := sock.frames()
		if len(got) != 3 || !strings.Contains(got[1], `"pong"`) || got[2] != `{"id":"1","type":"complete"}` {
			t.Fatalf("frames = %q, want the ack, then pong and complete under the reused id", got)
		}
	})
}

// An operation waiting for a slot is still the client's to complete, and one
// completed there never runs.
func TestAnOperationCompletedWhileWaitingForASlotNeverRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeafFixture(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{ slow }"}}`)
		done := serveWith(sock, Config{
			Exec: f.exec, InitTimeout: time.Minute, MaxSubs: 1, MaxConnectionIdle: 10 * time.Minute,
		})
		defer endServe(sock, done)
		defer f.open()
		synctest.Wait()
		sock.in <- []byte(completeOne)
		sock.in <- []byte(`{"id":"2","type":"subscribe","payload":{"query":"{ slow }"}}`)
		synctest.Wait()
		sock.in <- []byte(`{"id":"2","type":"complete"}`)
		synctest.Wait()

		f.open()
		synctest.Wait()
		if got := f.peak.Load(); got != 1 {
			t.Fatalf("peak = %d, want 1: the operation completed while waiting ran anyway", got)
		}
		if got := sock.frames(); len(got) != 1 {
			t.Fatalf("frames = %q, want only the ack", got)
		}

		// Nothing is left registered, so the connection goes idle.
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d, want %d", code, StatusNormalClosure)
		}
	})
}
