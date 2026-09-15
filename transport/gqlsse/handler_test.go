package gqlsse_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

const sdl = `
type Message { id: ID! body: String! fail: String! }
type Query { ping: String! }
type Mutation { touch: String! }
type Subscription { messages: Message! failing: Message! countdown(from: Int!): Int! }
`

type message struct {
	ID   string
	Body string
}

type fromArgs struct{ From int }

// source lets a test drive events from the test goroutine.
type source struct{ messages chan *message }

func newTestExecutor(t *testing.T) (*source, *graphql.Executor) {
	t.Helper()
	src := &source{messages: make(chan *message)}
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[message]("Message",
			graphql.Field("id", func(m *message) string { return m.ID }),
			graphql.Field("body", func(m *message) string { return m.Body }),
			graphql.Resolve("fail", func(context.Context, *message) (string, error) {
				return "", errBoom{}
			}),
		),
		graphql.Args[fromArgs](graphql.InputField("from", func(a *fromArgs, v int) { a.From = v })),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Mutation(graphql.Field("touch", func(graphql.Root) string { return "touched" })),
		graphql.Subscription(
			graphql.Subscribe("messages", func(context.Context) (<-chan *message, error) {
				return src.messages, nil
			}),
			graphql.Subscribe("failing", func(ctx context.Context) (<-chan *message, error) {
				ch := make(chan *message, 1)
				ch <- &message{ID: "1"}
				close(ch)
				return ch, nil
			}),
			graphql.SubscribeArgs("countdown", func(ctx context.Context, a fromArgs) (<-chan int, error) {
				ch := make(chan int)
				go func() {
					defer close(ch)
					for i := a.From; i > 0; i-- {
						select {
						case ch <- i:
						case <-ctx.Done():
							return
						}
					}
				}()
				return ch, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return src, graphql.NewExecutor(s)
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// event is one parsed SSE frame.
type event struct{ name, data string }

// readEvents parses the SSE stream until it ends, ignoring comment lines.
func readEvents(t *testing.T, body io.Reader) []event {
	t.Helper()
	var out []event
	var cur event
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.name != "" {
				out = append(out, cur)
				cur = event{}
			}
		case strings.HasPrefix(line, ":"):
			// keep-alive comment
		case strings.HasPrefix(line, "event:"):
			cur.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return out
}

// newServer starts a handler with its own client. Sharing http.DefaultClient
// across tests leaves pooled connections alive that httptest.Server.Close then
// waits on, which made the suite's runtime swing between 2 and 35 seconds.
func newServer(t *testing.T, e *graphql.Executor, opts ...gqlsse.Option) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(gqlsse.New(e, opts...))
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, client
}

func post(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestQueryStreamsNextThenComplete(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	resp := post(t, client, srv.URL, `{"query":"{ ping }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	got := readEvents(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if got[0].name != "next" || got[0].data != `{"data":{"ping":"pong"}}` {
		t.Fatalf("first event = %+v", got[0])
	}
	if got[1].name != "complete" {
		t.Fatalf("second event = %+v", got[1])
	}
}

func TestMutationOverPost(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	resp := post(t, client, srv.URL, `{"query":"mutation { touch }"}`)
	defer resp.Body.Close()
	got := readEvents(t, resp.Body)
	if len(got) != 2 || got[0].data != `{"data":{"touch":"touched"}}` {
		t.Fatalf("events = %+v", got)
	}
}

// TestSubscriptionStreamsEvents is the point of the transport: events arrive
// as they are produced, not batched at the end.
func TestSubscriptionStreamsEvents(t *testing.T) {
	src, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	resp := post(t, client, srv.URL, `{"query":"subscription { messages { id body } }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	done := make(chan []event, 1)
	go func() { done <- readEvents(t, resp.Body) }()

	src.messages <- &message{ID: "1", Body: "a"}
	src.messages <- &message{ID: "2", Body: "b"}
	close(src.messages)

	select {
	case got := <-done:
		want := []event{
			{"next", `{"data":{"messages":{"id":"1","body":"a"}}}`},
			{"next", `{"data":{"messages":{"id":"2","body":"b"}}}`},
			{"complete", ""},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d events: %+v", len(got), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading the stream")
	}
}

func TestSubscriptionArgumentsOverGet(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	resp, err := client.Get(srv.URL + "?query=" + url("subscription { countdown(from: 3) }"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := readEvents(t, resp.Body)
	want := []string{`{"data":{"countdown":3}}`, `{"data":{"countdown":2}}`, `{"data":{"countdown":1}}`}
	if len(got) != len(want)+1 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].name != "next" || got[i].data != w {
			t.Fatalf("event %d = %+v, want next %s", i, got[i], w)
		}
	}
	if got[len(got)-1].name != "complete" {
		t.Fatalf("stream did not complete: %+v", got)
	}
}

// TestSubscriptionFieldErrorStaysInBand checks that an error inside an event
// is a payload, not a transport failure: the stream keeps its 200 and still
// completes.
func TestSubscriptionFieldErrorStaysInBand(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	resp := post(t, client, srv.URL, `{"query":"subscription { failing { id fail } }"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := readEvents(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if !strings.Contains(got[0].data, `"errors"`) || !strings.Contains(got[0].data, `"data":null`) {
		t.Fatalf("first event should carry the error and a null payload: %s", got[0].data)
	}
	if got[1].name != "complete" {
		t.Fatalf("stream did not complete: %+v", got)
	}
}

// TestClientDisconnectStopsSubscription is the unsubscribe path over HTTP:
// closing the response body must end the operation rather than leave the
// source producing into a dead connection.
func TestClientDisconnectStopsSubscription(t *testing.T) {
	src, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	resp := post(t, client, srv.URL, `{"query":"subscription { messages { id } }"}`)

	// One event through, so the stream is certainly established.
	delivered := make(chan struct{})
	go func() {
		src.messages <- &message{ID: "1"}
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out delivering the first event")
	}
	resp.Body.Close()

	// The server detects the disconnect through its background read, which
	// costs a round trip, and an event already being received cannot be
	// un-received. So a few more may land; what must happen is that
	// consumption stops.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case src.messages <- &message{ID: "x"}:
		case <-time.After(250 * time.Millisecond):
			return // blocked: nothing is consuming any more
		}
	}
	t.Fatal("the executor was still consuming 5s after the client disconnected")
}

func TestKeepAliveComments(t *testing.T) {
	src, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false),
		gqlsse.WithKeepAlive(20*time.Millisecond))

	resp := post(t, client, srv.URL, `{"query":"subscription { messages { id } }"}`)
	defer resp.Body.Close()

	// Comments must appear on a stream that has produced no events.
	buf := make([]byte, 64)
	deadline := time.Now().Add(3 * time.Second)
	var seen string
	for time.Now().Before(deadline) && !strings.Contains(seen, ":") {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			seen += string(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(seen, ":") {
		t.Fatalf("no keep-alive comment on an idle stream, got %q", seen)
	}
	close(src.messages)
}

func TestRequestErrorsBeforeTheStream(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	tests := []struct {
		name, body string
		status     int
	}{
		{"parse error", `{"query":"subscription {"}`, http.StatusBadRequest},
		{"unknown field", `{"query":"subscription { nope }"}`, http.StatusBadRequest},
		{"query parse error", `{"query":"{ nope }"}`, http.StatusBadRequest},
		{"empty body", `{}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, client, srv.URL, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/graphql-response+json") {
				t.Fatalf("Content-Type = %q, want a GraphQL response", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), `"errors"`) {
				t.Fatalf("body carries no errors: %s", body)
			}
		})
	}
}

func TestTransportGuards(t *testing.T) {
	_, e := newTestExecutor(t)
	srv, client := newServer(t, e)
	noCSRF, _ := newServer(t, e, gqlsse.WithCSRFPrevention(false))

	t.Run("rejects a JSON-only Accept", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, noCSRF.URL, strings.NewReader(`{"query":"{ ping }"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotAcceptable {
			t.Fatalf("status = %d, want 406", resp.StatusCode)
		}
	})

	t.Run("rejects a forgeable request", func(t *testing.T) {
		resp, err := client.Post(srv.URL, "text/plain", strings.NewReader(`{"query":"{ ping }"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("rejects a mutation over GET", func(t *testing.T) {
		resp, err := client.Get(noCSRF.URL + "?query=" + url("mutation { touch }"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("rejects PUT", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, noCSRF.URL, strings.NewReader(`{"query":"{ ping }"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET, POST" {
			t.Fatalf("Allow = %q", allow)
		}
	})

	t.Run("rejects a non-JSON body", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, noCSRF.URL, strings.NewReader(`{"query":"{ ping }"}`))
		req.Header.Set("Content-Type", "application/graphql")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", resp.StatusCode)
		}
	})

	t.Run("rejects an oversized body", func(t *testing.T) {
		small, smallClient := newServer(t, e,
			gqlsse.WithCSRFPrevention(false), gqlsse.WithMaxBodyBytes(16))
		resp := post(t, smallClient, small.URL, `{"query":"{ ping }","operationName":"padding padding padding"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})
}

// url escapes a query for the GET form.
func url(q string) string { return neturl.QueryEscape(q) }

// TestPersistedSubscription checks that a subscription can be persisted too:
// the hash is resolved before the handler decides the operation is a
// subscription, so the stream opens exactly as it would with the text.
func TestPersistedSubscription(t *testing.T) {
	src, e := newTestExecutor(t)
	cache := apq.NewCache(10)
	srv, client := newServer(t, e,
		gqlsse.WithCSRFPrevention(false), gqlsse.WithPersistedQueries(cache))

	const query = `subscription { messages { id } }`
	hash := apq.Hash(query)
	ext := `{"persistedQuery":{"version":1,"sha256Hash":"` + hash + `"}}`

	// Unknown hash: no stream, and a body the client can act on.
	miss := post(t, client, srv.URL, `{"extensions":`+ext+`}`)
	body, _ := io.ReadAll(miss.Body)
	miss.Body.Close()
	if !strings.Contains(string(body), "PersistedQueryNotFound") {
		t.Fatalf("body = %s", body)
	}

	cache.Set(hash, query)
	resp := post(t, client, srv.URL, `{"extensions":`+ext+`}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want a stream", ct)
	}

	done := make(chan []event, 1)
	go func() { done <- readEvents(t, resp.Body) }()
	src.messages <- &message{ID: "1"}
	close(src.messages)

	select {
	case got := <-done:
		want := []event{{"next", `{"data":{"messages":{"id":"1"}}}`}, {"complete", ""}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading the persisted subscription")
	}
}
