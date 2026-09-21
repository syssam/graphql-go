package gqlwsproto

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// failWriteSocket fails exactly one write and then behaves normally, so a test
// can observe what the connection does after losing a message rather than
// losing every message after it. fakeSocket cannot fail a write at all, so
// without this nothing in this package reaches any write-failure branch.
type failWriteSocket struct {
	*fakeSocket
	failAt int

	mu     sync.Mutex
	writes int
}

func (f *failWriteSocket) Write(ctx context.Context, data []byte) error {
	f.mu.Lock()
	f.writes++
	n := f.writes
	f.mu.Unlock()
	if n == f.failAt {
		return errors.New("peer stopped reading")
	}
	return f.fakeSocket.Write(ctx, data)
}

// A subscription whose next cannot be written must have its id retired.
//
// Releasing the source is *not* what that guard does: subscribe launches the
// pump as `go func() { defer cancel(); c.run(...) }()`, so the source is freed
// when run returns however it returns. What only c.stop(id) does is delete the
// entry from c.subs -- and while that entry is there the connection believes an
// operation is in flight, so it never goes idle, the id cannot be reused, and
// the slot stays counted against MaxSubs. A connection that loses writes would
// leak all three for as long as it lives.
//
// Idle is the observable because it is the one this package can settle exactly:
// synctest makes the timer instant, where polling for a refusal races the pump
// and reads whichever answer arrives first.
//
// Held here because removing that stop leaves every other test in this package
// green; the only thing that noticed was TestCancelledConnectContextCloses over
// in transport/gqlws, which is about a different guard and whose name says
// nothing about this one.
func TestWriteFailureRetiresTheSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ticks := make(chan int)
		s, err := graphql.NewSchema(graphql.SDL(`
			type Query { ping: String! }
			type Subscription { tick: Int! }
		`),
			graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
			graphql.Subscription(graphql.Subscribe("tick", func(context.Context) (<-chan int, error) {
				return ticks, nil
			})),
		)
		if err != nil {
			t.Fatal(err)
		}

		// Write 1 is connection_ack; the first next is write 2 and is lost.
		sock := &failWriteSocket{fakeSocket: newFakeSocket(), failAt: 2}
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeSub)
		done := serveWith(sock, Config{
			Exec: graphql.NewExecutor(s), InitTimeout: time.Minute,
			MaxSubs: 10, MaxConnectionIdle: 10 * time.Minute,
		})

		ticks <- 1
		synctest.Wait()

		// The subscription is gone, so the connection is idle from here.
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d, want %d: a lost write left the subscription in c.subs, so the connection never went idle",
				code, StatusNormalClosure)
		}
		<-done
	})
}
