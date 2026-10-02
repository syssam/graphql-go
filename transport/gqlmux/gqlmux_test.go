package gqlmux_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/trusted"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlmux"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// newExecutor's subscription gives every subscriber a source of its own
// holding one tick, so no test's subscription can take another's event.
func newExecutor(t *testing.T, opts ...graphql.ExecutorOption) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { hello: String! }
		type Subscription { tick: Int! }
	`),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("tick", func(context.Context) (<-chan int, error) {
			ticks := make(chan int, 1)
			ticks <- 7
			return ticks, nil
		})),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return graphql.NewExecutor(s, opts...)
}

func post(t *testing.T, url, accept, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, strings.TrimSpace(string(b))
}

type wsFrame struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// dialWS opens a graphql-transport-ws connection and completes the handshake.
func dialWS(t *testing.T, url string) (func(wsFrame), func() wsFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"),
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	send := func(f wsFrame) {
		b, _ := json.Marshal(f)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	recv := func() wsFrame {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var f wsFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		return f
	}
	send(wsFrame{Type: "connection_init"})
	if got := recv(); got.Type != "connection_ack" {
		t.Fatalf("handshake returned %+v", got)
	}
	return send, recv
}

// A server that serves queries, SSE subscriptions and WebSockets used to be
// three handlers on three paths. One handler on one path serves all three,
// choosing by what the request itself asks for.
func TestOneHandlerServesEveryTransport(t *testing.T) {
	srv := httptest.NewServer(gqlmux.New(newExecutor(t)))
	defer srv.Close()

	t.Run("a query over HTTP", func(t *testing.T) {
		status, body := post(t, srv.URL, "", `{"query":"{ hello }"}`)
		if status != http.StatusOK || body != `{"data":{"hello":"world"}}` {
			t.Fatalf("answered %d %s", status, body)
		}
	})

	t.Run("a subscription over SSE", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"query":"subscription { tick }"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("Content-Type = %q, want an event stream", ct)
		}
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended before the event: %v", err)
			}
			if strings.HasPrefix(line, "data: ") {
				if got := strings.TrimSpace(line); got != `data: {"data":{"tick":7}}` {
					t.Fatalf("event = %s", got)
				}
				return
			}
		}
	})

	t.Run("a subscription over WebSocket", func(t *testing.T) {
		send, recv := dialWS(t, srv.URL)
		send(wsFrame{ID: "1", Type: "subscribe", Payload: json.RawMessage(`{"query":"subscription { tick }"}`)})
		if got := recv(); got.Type != "next" || string(got.Payload) != `{"data":{"tick":7}}` {
			t.Fatalf("frame = %+v", got)
		}
	})
}

// The reason to give an option once. A safelist handed to two of three
// handlers leaves the third answering any query at all, and the three
// separate constructors let exactly that be written.
func TestASafelistGivenOnceCoversEveryTransport(t *testing.T) {
	store := trusted.NewStore(map[string]string{"hello-v1": `{ hello }`})
	srv := httptest.NewServer(gqlmux.New(newExecutor(t, store.Enforce()...), gqlmux.WithPersistedQueries(store)))
	defer srv.Close()

	const registered = `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"hello-v1"}}}`
	const freeform = `{"query":"{ __typename }"}`

	// A registered id has to resolve on each transport, which is what the
	// option does there: Enforce on the executor refuses an unregistered
	// query everywhere on its own, so a refusal alone would not show that
	// any one transport had been given the store.
	if status, body := post(t, srv.URL, "", registered); status != http.StatusOK || body != `{"data":{"hello":"world"}}` {
		t.Errorf("HTTP, registered id: %d %s", status, body)
	}
	if _, body := post(t, srv.URL, "text/event-stream", registered); !strings.Contains(body, `{"data":{"hello":"world"}}`) {
		t.Errorf("SSE, registered id: %s", body)
	}
	send, recv := dialWS(t, srv.URL)
	send(wsFrame{ID: "1", Type: "subscribe", Payload: json.RawMessage(registered)})
	if got := recv(); got.Type != "next" || string(got.Payload) != `{"data":{"hello":"world"}}` {
		t.Errorf("WebSocket, registered id: %+v", got)
	}

	for _, accept := range []string{"", "text/event-stream"} {
		if _, body := post(t, srv.URL, accept, freeform); !strings.Contains(body, "PersistedQueryNotInList") {
			t.Errorf("Accept %q: an unregistered query was answered: %s", accept, body)
		}
	}
	send(wsFrame{ID: "2", Type: "subscribe", Payload: json.RawMessage(freeform)})
	for {
		got := recv()
		if got.ID != "2" {
			continue // the complete of operation 1
		}
		// A persisted-query refusal is sent as a result rather than as a
		// protocol error, so what matters is what it carries.
		if !strings.Contains(string(got.Payload), "PersistedQueryNotInList") || strings.Contains(string(got.Payload), `"data"`) {
			t.Errorf("WebSocket: an unregistered query was answered: %+v", got)
		}
		break
	}
}

// And the drain: one value ends both kinds of long-lived connection.
func TestADrainGivenOnceCoversBothStreamingTransports(t *testing.T) {
	d := drain.New()
	srv := httptest.NewServer(gqlmux.New(newExecutor(t), gqlmux.WithDrain(d)))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = d.Shutdown(ctx)

	if status, _ := post(t, srv.URL, "text/event-stream", `{"query":"subscription { tick }"}`); status != http.StatusServiceUnavailable {
		t.Errorf("a new SSE subscription during a drain answered %d, want 503", status)
	}
	_, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"),
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a new WebSocket during a drain: err=%v resp=%v, want 503", err, resp)
	}
	// Queries are ordinary requests the server's own Shutdown waits for.
	if status, body := post(t, srv.URL, "", `{"query":"{ hello }"}`); status != http.StatusOK {
		t.Errorf("a query during a drain answered %d %s", status, body)
	}
}

// An option for one transport overrides a shared one wherever it was written.
// That used to be worked out by counting which transports an option touched;
// it is now what WithHTTP, WithSSE and WithWS say they are.
func TestATransportsOwnOptionWinsInEitherOrder(t *testing.T) {
	const body = `{"query":"{ hello }"}`
	for name, opts := range map[string][]gqlmux.Option{
		"specific first": {gqlmux.WithHTTP(gqlhttp.WithMaxBodyBytes(1 << 20)), gqlmux.WithMaxBodyBytes(4)},
		"shared first":   {gqlmux.WithMaxBodyBytes(4), gqlmux.WithHTTP(gqlhttp.WithMaxBodyBytes(1 << 20))},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(gqlmux.New(newExecutor(t), opts...))
			defer srv.Close()
			if status, got := post(t, srv.URL, "", body); status != http.StatusOK {
				t.Errorf("HTTP answered %d %s: the shared four-byte limit overrode the transport's own", status, got)
			}
			// The shared limit still holds where nothing overrode it.
			if status, _ := post(t, srv.URL, "text/event-stream", body); status != http.StatusRequestEntityTooLarge {
				t.Errorf("SSE answered %d, want 413 from the shared limit", status)
			}
		})
	}
}
