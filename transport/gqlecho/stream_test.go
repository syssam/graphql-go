package gqlecho_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// newSubscriptionExecutor builds a schema with a subscription field driven by
// a test-controlled, unbuffered channel: a send only completes once the
// executor's subscribe loop has read it, so the test can pace events one at
// a time.
func newSubscriptionExecutor(t *testing.T) (chan string, *graphql.Executor) {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Subscription { ticks: String! }
`
	ch := make(chan string)
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(
			graphql.Subscribe("ticks", func(context.Context) (<-chan string, error) {
				return ch, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return ch, graphql.NewExecutor(s)
}

func TestSSEStreamsAQuery(t *testing.T) {
	e := echo.New()
	e.POST("/graphql", gqlecho.SSE(newTestExecutor(t)))

	srv := httptest.NewServer(e)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/graphql", strings.NewReader(`{"query":"{hello}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	// Read only the first data line and assert it decodes before the body is
	// closed: a scan that ran to EOF and grepped the whole buffer would pass
	// even if every write were buffered until the handler returned, which is
	// exactly the failure mode statusRecorder.Unwrap prevents (see the
	// package's gqlecho_test.go for why).
	var sawNext bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") && strings.Contains(sc.Text(), `"hello":"world"`) {
			sawNext = true
			break
		}
	}
	if !sawNext {
		t.Error("no data event carrying the result")
	}
}

// TestSSEStreamsSubscriptionEventsIncrementally is the strongest proof that
// SSE delegates to a real flushing stream: it reads the first event off the
// wire and only then publishes the second. ch is unbuffered, so the second
// send blocks until the executor's subscribe loop is ready for it -- but
// that says nothing about whether the first event ever left the server's
// buffer. If serve's statusRecorder hid the Flusher (see gqlecho_test.go),
// gqlsse's write of the first event would sit unflushed behind TCP/HTTP
// buffering with nothing forcing it out, sc.Scan below would block waiting
// for bytes that never arrive, and the test would time out rather than pass.
func TestSSEStreamsSubscriptionEventsIncrementally(t *testing.T) {
	ch, exec := newSubscriptionExecutor(t)
	e := echo.New()
	e.POST("/graphql", gqlecho.SSE(exec))

	srv := httptest.NewServer(e)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/graphql",
		strings.NewReader(`{"query":"subscription { ticks }"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	first := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				first <- line
				return
			}
		}
		first <- ""
	}()

	select {
	case ch <- "one":
	case <-time.After(5 * time.Second):
		t.Fatal("timed out publishing the first event")
	}

	var got string
	select {
	case got = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first event on the stream -- flushing is broken")
	}
	if !strings.Contains(got, `"ticks":"one"`) {
		t.Fatalf("first event = %q, want it to carry %q", got, "one")
	}

	// The client has consumed exactly one event so far. Publishing the
	// second only after asserting the first proves ordering, not just
	// eventual delivery.
	select {
	case ch <- "two":
	case <-time.After(5 * time.Second):
		t.Fatal("timed out publishing the second event")
	}
	close(ch)
}

// frame is one graphql-transport-ws protocol message.
type frame struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func TestWSHandshake(t *testing.T) {
	e := echo.New()
	e.GET("/graphql", gqlecho.WS(newTestExecutor(t)))

	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/graphql",
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	b, err := json.Marshal(frame{Type: "connection_init"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	if f.Type != "connection_ack" {
		t.Fatalf("frame = %+v, want connection_ack", f)
	}
}

// TestWSDrainClosesWithGoingAway proves gqlecho.WS inherits gqlws.WithDrain
// through its delegation to gqlws.Handler.ServeHTTP: a live subscription is
// cancelled and the socket closes 1001 once the drain shuts down.
func TestWSDrainClosesWithGoingAway(t *testing.T) {
	ch, exec := newSubscriptionExecutor(t)
	d := drain.New()
	e := echo.New()
	e.GET("/graphql", gqlecho.WS(exec, gqlws.WithDrain(d)))

	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/graphql",
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	b, err := json.Marshal(frame{Type: "connection_init"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, data, err := ws.Read(ctx); err != nil {
		t.Fatalf("read: %v", err)
	} else {
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		if f.Type != "connection_ack" {
			t.Fatalf("frame = %+v, want connection_ack", f)
		}
	}

	sb, err := json.Marshal(frame{ID: "1", Type: "subscribe", Payload: json.RawMessage(`{"query":"subscription { ticks }"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, sb); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case ch <- "one":
	case <-time.After(5 * time.Second):
		t.Fatal("timed out publishing the event")
	}
	if _, data, err := ws.Read(ctx); err != nil {
		t.Fatalf("read: %v", err)
	} else {
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		if f.Type != "next" {
			t.Fatalf("frame = %+v, want next", f)
		}
	}

	go func() { _ = d.Shutdown(context.Background()) }()

	_, _, err = ws.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("close status = %d, want %d", code, websocket.StatusGoingAway)
	}
}
