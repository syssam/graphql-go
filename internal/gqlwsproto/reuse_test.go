package gqlwsproto

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// reuseFixture is a schema whose query can be held open, so a test can
// complete an operation and reuse its id while the first is still running.
type reuseFixture struct {
	gate  chan struct{}
	ticks chan int
	live  atomic.Int64
	exec  *graphql.Executor
}

func newReuseFixture(t *testing.T, opts ...graphql.ExecutorOption) *reuseFixture {
	t.Helper()
	f := &reuseFixture{gate: make(chan struct{}), ticks: make(chan int)}
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { ping: String! slow: String! }
		type Subscription { tick: Int! }
	`),
		graphql.Query(
			graphql.Field("ping", func(graphql.Root) string { return "pong" }),
			// Deliberately deaf to its context: an operation the client has
			// completed may still be running, which is the case under test.
			graphql.Resolve("slow", func(context.Context, graphql.Root) (string, error) {
				<-f.gate
				return "stale", nil
			}),
		),
		graphql.Subscription(graphql.Subscribe("tick", func(ctx context.Context) (<-chan int, error) {
			f.live.Add(1)
			context.AfterFunc(ctx, func() { f.live.Add(-1) })
			return f.ticks, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.exec = graphql.NewExecutor(s, opts...)
	return f
}

func (f *fakeSocket) frames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.out))
	for _, b := range f.out {
		out = append(out, string(b))
	}
	return out
}

const completeOne = `{"id":"1","type":"complete"}`

// The protocol lets a client reuse an id as soon as it has completed it, and
// an operation is tracked by nothing but that id. A query still running when
// its id was reused therefore answered into the operation that now owned it.
func TestACompletedQueryDoesNotAnswerUnderItsReusedID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newReuseFixture(t)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{ slow }"}}`)
		done := serveWith(sock, Config{Exec: f.exec, InitTimeout: time.Minute, MaxSubs: 10})
		synctest.Wait()

		sock.in <- []byte(completeOne)
		sock.in <- []byte(subscribeSub)
		synctest.Wait()
		f.ticks <- 7
		synctest.Wait()
		close(f.gate)
		synctest.Wait()

		for _, m := range sock.frames() {
			if strings.Contains(m, "stale") {
				t.Fatalf("the completed query's result was sent under the id a subscription now owns: %s", m)
			}
		}
		if got := sock.frames(); len(got) != 2 || !strings.Contains(got[1], `"tick":7`) {
			t.Fatalf("frames = %v, want the ack and one tick", got)
		}

		_ = sock.Close(StatusNormalClosure, "done")
		<-done
	})
}

// The same race through the terminal path was worse: the stale operation's
// error retired the id, which deleted the entry of the subscription that had
// reused it. That subscription kept running where complete could no longer
// reach it and MaxSubs no longer counted it.
func TestACompletedOperationDoesNotRetireItsReusedID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		f := newReuseFixture(t, graphql.WithRequestInterceptor(graphql.RequestInterceptorFunc(
			func(ctx context.Context, req *graphql.Request, next graphql.RequestHandler) *graphql.Response {
				if req.OperationName == "Old" {
					<-release
					return &graphql.Response{Errors: []*graphql.Error{graphql.Errorf("refused")}}
				}
				return next(ctx, req)
			})))
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"query Old { ping }","operationName":"Old"}}`)
		done := serveWith(sock, Config{Exec: f.exec, InitTimeout: time.Minute, MaxSubs: 1})
		synctest.Wait()

		sock.in <- []byte(completeOne)
		sock.in <- []byte(subscribeSub)
		synctest.Wait()
		close(release)
		synctest.Wait()

		for _, m := range sock.frames() {
			if strings.Contains(m, "refused") {
				t.Fatalf("the completed operation's error was sent under the id a subscription now owns: %s", m)
			}
		}

		// The subscription still holds the connection's one slot.
		sock.in <- []byte(`{"id":"2","type":"subscribe","payload":{"query":"{ ping }"}}`)
		synctest.Wait()
		got := sock.frames()
		if last := got[len(got)-1]; !strings.Contains(last, `"id":"2"`) || !strings.Contains(last, "at most 1 operations") {
			t.Fatalf("a second operation was admitted on a connection capped at one: %v", got)
		}

		// And the client can still end it.
		sock.in <- []byte(completeOne)
		synctest.Wait()
		if n := f.live.Load(); n != 0 {
			t.Fatalf("%d subscription sources still open after the client completed id 1", n)
		}

		_ = sock.Close(StatusNormalClosure, "done")
		<-done
	})
}
