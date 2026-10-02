package gqlsse_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

func quietSubscription(t *testing.T) (*graphql.Executor, chan int) {
	t.Helper()
	ticks := make(chan int)
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { ping: String! }
		type Subscription { tick: Int! }
	`),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Subscription(graphql.Subscribe("tick", func(context.Context) (<-chan int, error) { return ticks, nil })),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return graphql.NewExecutor(s), ticks
}

func openStream(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"query":"subscription { tick }"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return resp
}

// The write timeout bounds a write. Left set after the write, it is a deadline
// on the stream: on HTTP/2 the server resets a stream whose write deadline
// passes whether or not anything is being written, so a subscription with
// nothing to say for longer than the timeout was cut off.
func TestTheWriteTimeoutDoesNotEndAQuietStream(t *testing.T) {
	for _, proto := range []string{"HTTP/1.1", "HTTP/2"} {
		t.Run(proto, func(t *testing.T) {
			e, ticks := quietSubscription(t)
			srv := httptest.NewUnstartedServer(gqlsse.New(e,
				gqlsse.WithWriteTimeout(100*time.Millisecond), gqlsse.WithKeepAlive(0)))
			if proto == "HTTP/2" {
				srv.EnableHTTP2 = true
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()

			resp := openStream(t, srv.Client(), srv.URL)
			// Deferred after srv.Close, so it runs first: Close waits for
			// open streams, and this one ends only when its client leaves.
			defer func() { _ = resp.Body.Close() }()
			if proto == "HTTP/2" && resp.ProtoMajor != 2 {
				t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
			}
			// Quiet for several timeouts, then one event.
			time.Sleep(400 * time.Millisecond)
			go func() { ticks <- 7 }()

			lines := make(chan string, 1)
			go func() {
				r := bufio.NewReader(resp.Body)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						lines <- "read error: " + err.Error()
						return
					}
					if strings.HasPrefix(line, "data: ") {
						lines <- strings.TrimSpace(line)
						return
					}
				}
			}()
			select {
			case got := <-lines:
				if got != `data: {"data":{"tick":7}}` {
					t.Fatalf("after a quiet spell the stream gave %q, want the event", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no event arrived")
			}
		})
	}
}

// And a stream that ends after a quiet spell -- by its age limit here -- ends
// as a complete response. With the deadline of an earlier write still set, the
// terminating chunk was written past it and failed, so the client read a
// truncated body.
func TestAStreamEndsCleanlyAfterAQuietSpell(t *testing.T) {
	e, _ := quietSubscription(t)
	srv := httptest.NewServer(gqlsse.New(e, gqlsse.WithWriteTimeout(100*time.Millisecond),
		gqlsse.WithKeepAlive(0), gqlsse.WithMaxStreamAge(400*time.Millisecond)))
	defer srv.Close()

	resp := openStream(t, srv.Client(), srv.URL)
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("the stream did not end as a complete response: %v", err)
	}
}
