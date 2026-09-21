package gqlwsproto

import (
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// A subscribe carrying only a hash has no query text. ResolvePersisted either
// fills it in or hands back the response to send.
const subscribeHashOnly = `{"id":"p","type":"subscribe","payload":{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"deadbeef"}}}}`

// A persisted-query miss must reach the client as an ordinary result for that
// operation -- next then complete -- and never as an error message. The whole
// APQ handshake depends on the client seeing PersistedQueryNotFound and
// retrying with the full query text; graphql-ws treats error as terminal and
// will not retry, so sending one silently breaks the protocol this implements.
func TestPersistedMissIsNextAndComplete(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(initMsg)
	sock.in <- []byte(subscribeHashOnly)

	miss := &graphql.Response{Errors: []*graphql.Error{
		graphql.Errorf("PersistedQueryNotFound").WithCode("PERSISTED_QUERY_NOT_FOUND"),
	}}
	done := serveWith(sock, Config{
		InitTimeout: time.Minute,
		MaxSubs:     10,
		ResolvePersisted: func(*graphql.Request) *graphql.Response {
			return miss
		},
	})
	time.Sleep(50 * time.Millisecond)
	_ = sock.Close(1000, "done")
	<-done

	got := sock.types()
	if slicesContains(got, "error") {
		t.Fatalf("message types = %v: a persisted miss was sent as error, which graphql-ws will not retry", got)
	}
	if !slicesContains(got, "next") || !slicesContains(got, "complete") {
		t.Fatalf("message types = %v, want next then complete", got)
	}
	if !wrote(sock, "PersistedQueryNotFound") {
		t.Fatal("the miss response never reached the client")
	}
}

// A hook that fills in the query lets the operation run normally, which is the
// registered-hash path.
func TestPersistedHitRunsTheOperation(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(initMsg)
	sock.in <- []byte(subscribeHashOnly)

	var saw *graphql.Request
	done := serveWith(sock, Config{
		Exec:        newPersistedExecutor(t),
		InitTimeout: time.Minute,
		MaxSubs:     10,
		ResolvePersisted: func(r *graphql.Request) *graphql.Response {
			saw = r
			r.Query = "{ hello }"
			return nil
		},
	})
	time.Sleep(50 * time.Millisecond)
	_ = sock.Close(1000, "done")
	<-done

	if saw == nil {
		t.Fatal("ResolvePersisted was never called")
	}
	if saw.Extensions == nil {
		t.Fatal("the hook did not see the subscribe payload's extensions, where persistedQuery lives")
	}
	if !wrote(sock, "world") {
		t.Fatalf("the filled-in query did not run; types = %v", sock.types())
	}
}

// With no hook configured, a subscribe behaves exactly as before.
func TestNoPersistedHookIsUnchanged(t *testing.T) {
	sock := newFakeSocket()
	sock.in <- []byte(initMsg)
	sock.in <- []byte(`{"id":"1","type":"subscribe","payload":{"query":"{ hello }"}}`)

	done := serveWith(sock, Config{Exec: newPersistedExecutor(t), InitTimeout: time.Minute, MaxSubs: 10})
	time.Sleep(50 * time.Millisecond)
	_ = sock.Close(1000, "done")
	<-done

	if !wrote(sock, "world") {
		t.Fatalf("an ordinary subscribe stopped working; types = %v", sock.types())
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func newPersistedExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(`type Query { hello: String! }`),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })))
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}
