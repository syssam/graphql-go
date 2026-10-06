package gqlwsproto

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// unserializableExecutor answers every operation with an extension
// encoding/json refuses, which is the one way a response fails to marshal.
func unserializableExecutor(t *testing.T, ticks <-chan int) *graphql.Executor {
	t.Helper()
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
	return graphql.NewExecutor(s, graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(
		func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
			oc.SetExtension("bad", func() {})
			return next(ctx, oc)
		})))
}

// endServe closes the socket and waits for Serve, so a test that fails half
// way leaves nothing blocked in its bubble.
func endServe(sock *fakeSocket, done <-chan struct{}) {
	_ = sock.Close(StatusNormalClosure, "done")
	<-done
}

const fallbackNext = `{"id":"1","type":"next","payload":{"errors":[{"message":"internal system error"}]}}`

// A response that will not serialize used to be dropped: no next, no error, no
// complete and no log line, so a query's client waited for ever and a
// subscription ended on its first event while its client went on listening.
// The HTTP and SSE handlers send a fallback envelope for the same failure.
func TestAQueryThatWillNotSerializeIsStillAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{ ping }"}}`)
		done := serveWith(sock, Config{
			Exec: unserializableExecutor(t, nil), InitTimeout: time.Minute, MaxSubs: 1,
			Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		})
		defer endServe(sock, done)
		synctest.Wait()

		got := sock.frames()
		if len(got) != 3 || got[1] != fallbackNext || got[2] != `{"id":"1","type":"complete"}` {
			t.Fatalf("frames = %q, want the ack, a next carrying the fallback and a complete", got)
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "serializing") {
			t.Errorf("the failure was not logged: %q", logs.String())
		}

		// The one slot is free again.
		sock.in <- []byte(`{"id":"2","type":"subscribe","payload":{"query":"{ ping }"}}`)
		synctest.Wait()
		if last := sock.frames(); !strings.Contains(last[len(last)-1], `"id":"2","type":"complete"`) {
			t.Fatalf("a second operation was not served after the first: %q", last)
		}
	})
}

// On a subscription the event is answered and the stream goes on, as it does
// over SSE: one event that will not serialize is not the end of the source.
func TestASubscriptionEventThatWillNotSerializeDoesNotEndTheStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ticks := make(chan int)
		sock := newFakeSocket()
		sock.in <- []byte(initMsg)
		sock.in <- []byte(subscribeSub)
		done := serveWith(sock, Config{
			Exec: unserializableExecutor(t, ticks), InitTimeout: time.Minute, MaxSubs: 1,
			MaxConnectionIdle: 10 * time.Minute,
			Logger:            slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)),
		})
		defer endServe(sock, done)

		ticks <- 1
		ticks <- 2
		synctest.Wait()
		if got := sock.frames(); len(got) != 3 || got[1] != fallbackNext || got[2] != fallbackNext {
			t.Fatalf("frames = %q, want the ack and a fallback next for each event", got)
		}

		close(ticks)
		synctest.Wait()
		if got := sock.frames(); len(got) != 4 || got[3] != `{"id":"1","type":"complete"}` {
			t.Fatalf("frames = %q, want a complete once the source ended", got)
		}

		// Retired like any other: the connection is idle from here.
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if code := sock.closeCode(); code != StatusNormalClosure {
			t.Fatalf("close code = %d, want %d: the subscription was never retired", code, StatusNormalClosure)
		}
	})
}
