package gqlws_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlws"
)

const sdl = `
type Message { id: ID! body: String! fail: String! seq: Int! }
type Query { ping: String! whoami: String! }
type Mutation { touch: String! }
type Subscription { messages: Message! failing: Message! countdown(from: Int!): Int! }
`

type message struct {
	ID   string
	Body string
}

type fromArgs struct{ From int }

type userKey struct{}

type source struct{ messages chan *message }

func newTestExecutor(t *testing.T) (*source, *graphql.Executor) {
	t.Helper()
	src := &source{messages: make(chan *message)}
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[message]("Message",
			graphql.Field("id", func(m *message) string { return m.ID }),
			graphql.Field("body", func(m *message) string { return m.Body }),
			graphql.Resolve("fail", func(context.Context, *message) (string, error) {
				return "", errors.New("boom")
			}),
			// seq reaches its operation only through the context, counts its runs
			// there and republishes the count as an extension. Both halves fail
			// visibly if events share one operation context.
			graphql.Resolve("seq", func(ctx context.Context, _ *message) (int, error) {
				oc := graphql.OperationFrom(ctx)
				if oc == nil {
					return 0, errors.New("no operation on the resolver context")
				}
				actual, _ := oc.GetOrSet("seq", new(int))
				n := actual.(*int)
				*n++
				oc.SetExtension("seq", *n)
				return *n, nil
			}),
		),
		graphql.Args[fromArgs](graphql.InputField("from", func(a *fromArgs, v int) { a.From = v })),
		graphql.Query(
			graphql.Field("ping", func(graphql.Root) string { return "pong" }),
			// whoami reads what ConnectFunc put on the connection context, so a
			// test can prove the hook's context reaches resolvers.
			graphql.Resolve("whoami", func(ctx context.Context, _ graphql.Root) (string, error) {
				name, _ := ctx.Value(userKey{}).(string)
				if name == "" {
					name = "anonymous"
				}
				return name, nil
			}),
		),
		graphql.Mutation(graphql.Field("touch", func(graphql.Root) string { return "touched" })),
		graphql.Subscription(
			graphql.Subscribe("messages", func(context.Context) (<-chan *message, error) {
				return src.messages, nil
			}),
			graphql.Subscribe("failing", func(context.Context) (<-chan *message, error) {
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

// frame is one protocol message.
type frame struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// client is a minimal graphql-transport-ws client.
type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func dial(t *testing.T, e *graphql.Executor, opts ...gqlws.Option) *client {
	t.Helper()
	srv := httptest.NewServer(gqlws.New(e, opts...))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"),
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &client{t: t, ws: ws}
}

func (c *client) send(f frame) {
	c.t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) recv() frame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		c.t.Fatalf("decode %s: %v", data, err)
	}
	return f
}

// recvErr expects the connection to close and returns the status code.
func (c *client) recvErr() websocket.StatusCode {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.ws.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// init performs the handshake and asserts the ack.
func (c *client) init(payload string) {
	c.t.Helper()
	f := frame{Type: "connection_init"}
	if payload != "" {
		f.Payload = json.RawMessage(payload)
	}
	c.send(f)
	if got := c.recv(); got.Type != "connection_ack" {
		c.t.Fatalf("handshake returned %+v, want connection_ack", got)
	}
}

func (c *client) subscribe(id, query string) {
	c.t.Helper()
	c.send(frame{ID: id, Type: "subscribe", Payload: json.RawMessage(`{"query":` + quote(query) + `}`)})
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestQueryIsNextThenComplete(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("1", `{ ping }`)
	if got := c.recv(); got.Type != "next" || got.ID != "1" || string(got.Payload) != `{"data":{"ping":"pong"}}` {
		t.Fatalf("first frame = %+v", got)
	}
	if got := c.recv(); got.Type != "complete" || got.ID != "1" {
		t.Fatalf("second frame = %+v", got)
	}
}

func TestMutationIsNextThenComplete(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("m", `mutation { touch }`)
	if got := c.recv(); string(got.Payload) != `{"data":{"touch":"touched"}}` {
		t.Fatalf("frame = %+v", got)
	}
	if got := c.recv(); got.Type != "complete" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestSubscriptionStreamsEvents(t *testing.T) {
	src, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("s", `subscription { messages { id body } }`)
	go func() {
		src.messages <- &message{ID: "1", Body: "a"}
		src.messages <- &message{ID: "2", Body: "b"}
		close(src.messages)
	}()

	for _, want := range []string{
		`{"data":{"messages":{"id":"1","body":"a"}}}`,
		`{"data":{"messages":{"id":"2","body":"b"}}}`,
	} {
		got := c.recv()
		if got.Type != "next" || got.ID != "s" || string(got.Payload) != want {
			t.Fatalf("frame = %+v, want next %s", got, want)
		}
	}
	if got := c.recv(); got.Type != "complete" || got.ID != "s" {
		t.Fatalf("frame = %+v, want complete", got)
	}
}

func TestSubscriptionArguments(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("c", `subscription { countdown(from: 3) }`)
	for _, want := range []string{`{"data":{"countdown":3}}`, `{"data":{"countdown":2}}`, `{"data":{"countdown":1}}`} {
		if got := c.recv(); got.Type != "next" || string(got.Payload) != want {
			t.Fatalf("frame = %+v, want next %s", got, want)
		}
	}
	if got := c.recv(); got.Type != "complete" {
		t.Fatalf("frame = %+v", got)
	}
}

// TestClientCompleteUnsubscribes is the cancellation path: a complete from the
// client stops the operation and must not be answered with a complete back.
func TestClientCompleteUnsubscribes(t *testing.T) {
	src, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("s", `subscription { messages { id } }`)
	src.messages <- &message{ID: "1"}
	if got := c.recv(); got.Type != "next" {
		t.Fatalf("frame = %+v", got)
	}

	c.send(frame{ID: "s", Type: "complete"})

	// The executor stops consuming, so sends find no receiver. The complete
	// may still be in flight when the first of these is sent, so require only
	// that consumption stops.
	stopped := false
	deadline := time.Now().Add(5 * time.Second)
	for !stopped && time.Now().Before(deadline) {
		select {
		case src.messages <- &message{ID: "x"}:
		case <-time.After(250 * time.Millisecond):
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("the executor was still consuming after the client sent complete")
	}

	// A real client ignores frames for an id it has completed, because one can
	// always be mid-write when the complete arrives. Drain any, then check the
	// connection still serves new operations.
	c.subscribe("2", `{ ping }`)
	for {
		got := c.recv()
		if got.ID == "s" {
			if got.Type != "next" {
				t.Fatalf("frame for a completed id should only ever be a stale next: %+v", got)
			}
			continue
		}
		if got.Type != "next" || got.ID != "2" {
			t.Fatalf("connection unusable after unsubscribe: %+v", got)
		}
		return
	}
}

// TestOperationErrorIsTerminal checks the protocol's distinction: an operation
// that never ran produces error, not next, and nothing follows it.
func TestOperationErrorIsTerminal(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("bad", `subscription { nope }`)
	got := c.recv()
	if got.Type != "error" || got.ID != "bad" {
		t.Fatalf("frame = %+v, want error", got)
	}
	var errs []map[string]any
	if err := json.Unmarshal(got.Payload, &errs); err != nil {
		t.Fatalf("error payload is not an array of errors: %s", got.Payload)
	}
	if len(errs) == 0 {
		t.Fatalf("error payload is empty: %s", got.Payload)
	}

	// The id is retired by the error, so the client may reuse it immediately.
	c.subscribe("bad", `{ ping }`)
	if got := c.recv(); got.Type != "next" || got.ID != "bad" {
		t.Fatalf("id was not released after error: %+v", got)
	}
}

// TestFieldErrorStaysInBand is the other half: an error raised during
// execution is a payload on a next, and the operation still completes.
func TestFieldErrorStaysInBand(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("f", `subscription { failing { id fail } }`)
	got := c.recv()
	if got.Type != "next" {
		t.Fatalf("frame = %+v, want next", got)
	}
	if !strings.Contains(string(got.Payload), `"errors"`) {
		t.Fatalf("payload carries no errors: %s", got.Payload)
	}
	if next := c.recv(); next.Type != "complete" {
		t.Fatalf("frame = %+v, want complete", next)
	}
}

func TestPingIsAnsweredWithPong(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.send(frame{Type: "ping", Payload: json.RawMessage(`{"n":1}`)})
	got := c.recv()
	if got.Type != "pong" {
		t.Fatalf("frame = %+v, want pong", got)
	}
	if string(got.Payload) != `{"n":1}` {
		t.Fatalf("pong did not echo the payload: %s", got.Payload)
	}
}

// TestPingBeforeInit covers the one message allowed before the handshake.
func TestPingBeforeInit(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e)

	c.send(frame{Type: "ping"})
	if got := c.recv(); got.Type != "pong" {
		t.Fatalf("frame = %+v, want pong", got)
	}
	c.init("")
}

func TestOnConnectContextReachesResolvers(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithOnConnect(func(ctx context.Context, payload []byte) (context.Context, error) {
		var p struct {
			User string `json:"user"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		if p.User == "" {
			return nil, errors.New("missing user")
		}
		if gqlws.RequestFrom(ctx) == nil {
			return nil, errors.New("the upgrade request should be available to the hook")
		}
		return context.WithValue(ctx, userKey{}, p.User), nil
	}))
	c.init(`{"user":"ada"}`)

	c.subscribe("1", `{ whoami }`)
	if got := c.recv(); string(got.Payload) != `{"data":{"whoami":"ada"}}` {
		t.Fatalf("frame = %+v", got)
	}
}

func TestProtocolViolations(t *testing.T) {
	_, e := newTestExecutor(t)

	t.Run("message before init is unauthorized", func(t *testing.T) {
		c := dial(t, e)
		c.subscribe("1", `{ ping }`)
		if code := c.recvErr(); code != gqlws.StatusUnauthorized {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusUnauthorized)
		}
	})

	t.Run("second init is too many requests", func(t *testing.T) {
		c := dial(t, e)
		c.init("")
		c.send(frame{Type: "connection_init"})
		if code := c.recvErr(); code != gqlws.StatusTooManyInitRequests {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusTooManyInitRequests)
		}
	})

	t.Run("duplicate id", func(t *testing.T) {
		src, e := newTestExecutor(t)
		c := dial(t, e)
		c.init("")
		c.subscribe("dup", `subscription { messages { id } }`)
		src.messages <- &message{ID: "1"}
		if got := c.recv(); got.Type != "next" {
			t.Fatalf("frame = %+v", got)
		}
		c.subscribe("dup", `subscription { messages { id } }`)
		if code := c.recvErr(); code != gqlws.StatusSubscriberExists {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusSubscriberExists)
		}
	})

	t.Run("unknown message type", func(t *testing.T) {
		c := dial(t, e)
		c.init("")
		c.send(frame{Type: "nonsense"})
		if code := c.recvErr(); code != gqlws.StatusBadRequest {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusBadRequest)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		c := dial(t, e)
		c.init("")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.ws.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
			t.Fatal(err)
		}
		if code := c.recvErr(); code != gqlws.StatusBadRequest {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusBadRequest)
		}
	})

	t.Run("subscribe without an id", func(t *testing.T) {
		c := dial(t, e)
		c.init("")
		c.send(frame{Type: "subscribe", Payload: json.RawMessage(`{"query":"{ ping }"}`)})
		if code := c.recvErr(); code != gqlws.StatusBadRequest {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusBadRequest)
		}
	})

	t.Run("rejected by OnConnect", func(t *testing.T) {
		c := dial(t, e, gqlws.WithOnConnect(func(context.Context, []byte) (context.Context, error) {
			return nil, errors.New("nope")
		}))
		c.send(frame{Type: "connection_init"})
		if code := c.recvErr(); code != gqlws.StatusForbidden {
			t.Fatalf("close code = %d, want %d", code, gqlws.StatusForbidden)
		}
	})
}

func TestInitTimeout(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithInitTimeout(50*time.Millisecond))
	if code := c.recvErr(); code != gqlws.StatusInitTimeout {
		t.Fatalf("close code = %d, want %d", code, gqlws.StatusInitTimeout)
	}
}

func TestMaxSubscriptionsKeepsTheConnection(t *testing.T) {
	src, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithMaxSubscriptions(1))
	c.init("")

	c.subscribe("a", `subscription { messages { id } }`)
	src.messages <- &message{ID: "1"}
	if got := c.recv(); got.Type != "next" {
		t.Fatalf("frame = %+v", got)
	}

	c.subscribe("b", `subscription { messages { id } }`)
	got := c.recv()
	if got.Type != "error" || got.ID != "b" {
		t.Fatalf("frame = %+v, want an error for b", got)
	}
	if !strings.Contains(string(got.Payload), "at most 1") {
		t.Fatalf("error does not explain the cap: %s", got.Payload)
	}

	// The first subscription is untouched.
	src.messages <- &message{ID: "2"}
	if next := c.recv(); next.Type != "next" || next.ID != "a" {
		t.Fatalf("frame = %+v, want next for a", next)
	}
}

func TestServerPings(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithPingInterval(30*time.Millisecond))
	c.init("")
	if got := c.recv(); got.Type != "ping" {
		t.Fatalf("frame = %+v, want a server ping", got)
	}
}

func TestSubprotocolRequired(t *testing.T) {
	_, e := newTestExecutor(t)
	srv := httptest.NewServer(gqlws.New(e))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		// Some stacks refuse the handshake outright, which is also correct.
		return
	}
	defer ws.CloseNow()
	_, _, rerr := ws.Read(ctx)
	if code := websocket.CloseStatus(rerr); code != gqlws.StatusSubprotocolNotAcceptable {
		t.Fatalf("close code = %d, want %d", code, gqlws.StatusSubprotocolNotAcceptable)
	}
}

func TestRejectsPlainHTTP(t *testing.T) {
	_, e := newTestExecutor(t)
	srv := httptest.NewServer(gqlws.New(e))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a non-upgrade request should not succeed")
	}
}

// TestEventContextIsPerEventOverTheWire is the WebSocket half of the same
// guarantee: each event carries its own operation context, so request-scoped
// resolver state does not leak between events and the extensions a resolver
// sets reach that event's payload. A DataLoader depends on both.
func TestEventContextIsPerEventOverTheWire(t *testing.T) {
	src, e := newTestExecutor(t)
	c := dial(t, e)
	c.init("")

	c.subscribe("s", `subscription { messages { id seq } }`)
	go func() {
		src.messages <- &message{ID: "1"}
		src.messages <- &message{ID: "2"}
		close(src.messages)
	}()

	for _, want := range []string{
		`{"data":{"messages":{"id":"1","seq":1}},"extensions":{"seq":1}}`,
		`{"data":{"messages":{"id":"2","seq":1}},"extensions":{"seq":1}}`,
	} {
		got := c.recv()
		if got.Type != "next" || string(got.Payload) != want {
			t.Fatalf("frame = %+v\nwant next %s\n(a seq above 1, or missing extensions, means the events shared one operation context)", got, want)
		}
	}
	if got := c.recv(); got.Type != "complete" {
		t.Fatalf("frame = %+v, want complete", got)
	}
}

// TestDrainClosesWithGoingAway proves WithDrain reaches the connection: a
// live subscription is cancelled and the socket closes 1001, and Shutdown
// itself returns promptly rather than waiting out its whole deadline.
func TestDrainClosesWithGoingAway(t *testing.T) {
	src, e := newTestExecutor(t)
	d := drain.New()
	c := dial(t, e, gqlws.WithDrain(d))
	c.init("")

	c.subscribe("1", `subscription { messages { id } }`)
	src.messages <- &message{ID: "1"}
	if got := c.recv(); got.Type != "next" {
		t.Fatalf("frame = %+v", got)
	}

	type result struct {
		err error
		dur time.Duration
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := d.Shutdown(ctx)
		done <- result{err, time.Since(start)}
	}()

	if code := c.recvErr(); code != websocket.StatusGoingAway {
		t.Fatalf("close code = %d, want %d", code, websocket.StatusGoingAway)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Shutdown returned %v", r.err)
		}
		if r.dur > 2*time.Second {
			t.Fatalf("Shutdown took %v, want under 2s", r.dur)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}

// TestDrainWaitsForConnectionThenGivesUp proves the connection is registered
// with the drain and runs under the context the drain cancels. A query that
// ignores Closing keeps the connection's drain waiting, so Shutdown returns
// DeadlineExceeded only if it was waiting for this connection, and the client
// gets 1001 only if giving up reaches the protocol. Closing alone produces
// neither.
//
// The 1001 is the part coder/websocket makes easy to lose: it closes the
// connection with no frame as soon as a read's context is cancelled, so a
// protocol reading under the context the drain cancels ends the connection
// before its own close frame is written.
func TestDrainWaitsForConnectionThenGivesUp(t *testing.T) {
	started := make(chan struct{}, 1)
	s, err := graphql.NewSchema(graphql.SDL(`type Query { block: String! }`),
		graphql.Query(graphql.Resolve("block", func(ctx context.Context, _ graphql.Root) (string, error) {
			started <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		})),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	d := drain.New()
	c := dial(t, graphql.NewExecutor(s), gqlws.WithDrain(d))
	c.init("")

	c.subscribe("1", `{ block }`)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the query never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}

	if code := c.recvErr(); code != gqlws.StatusGoingAway {
		t.Fatalf("close code = %d after Shutdown gave up, want %d", code, gqlws.StatusGoingAway)
	}
}

// TestDrainRefusesNewConnections proves the ServeHTTP entry point itself
// refuses the upgrade once draining has begun, rather than only tearing down
// connections already open.
func TestDrainRefusesNewConnections(t *testing.T) {
	_, e := newTestExecutor(t)
	d := drain.New()
	srv := httptest.NewServer(gqlws.New(e, gqlws.WithDrain(d)))
	defer srv.Close()

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"),
		&websocket.DialOptions{Subprotocols: []string{gqlws.Subprotocol}})
	if err == nil {
		t.Fatal("dial succeeded while the drain was shutting down")
	}
	if resp == nil {
		t.Fatal("no response returned for the refused upgrade")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestCancelledConnectContextCloses: the context OnConnect returns can end on
// its own, as an expiring token would. The connection must close 1001 then,
// so the client reconnects and re-authenticates, rather than stay open with
// nothing served on it.
func TestCancelledConnectContextCloses(t *testing.T) {
	_, e := newTestExecutor(t)
	session, expire := context.WithCancel(context.Background())
	defer expire()
	c := dial(t, e, gqlws.WithOnConnect(func(ctx context.Context, _ []byte) (context.Context, error) {
		ctx, cancel := context.WithCancel(ctx)
		context.AfterFunc(session, cancel)
		return ctx, nil
	}))
	c.init("")

	expire()
	if code := c.recvErr(); code != gqlws.StatusGoingAway {
		t.Fatalf("close code = %d after the connection context ended, want %d", code, gqlws.StatusGoingAway)
	}
}

// RequestFrom is documented as available to a ConnectFunc and to nothing
// later, because the request is finished by then. But the idiomatic hook
// returns a context derived from the one it was given, and that became the
// parent of every operation: the request reached every resolver.
func TestTheUpgradeRequestDoesNotReachOperations(t *testing.T) {
	s, err := graphql.NewSchema(graphql.SDL(`type Query { seen: String! }`),
		graphql.Query(graphql.Resolve("seen", func(ctx context.Context, _ graphql.Root) (string, error) {
			name, _ := ctx.Value(userKey{}).(string)
			if gqlws.RequestFrom(ctx) != nil {
				return name + ": the upgrade request", nil
			}
			return name + ": nothing", nil
		})))
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	sawRequest := false
	c := dial(t, graphql.NewExecutor(s), gqlws.WithOnConnect(func(ctx context.Context, _ []byte) (context.Context, error) {
		sawRequest = gqlws.RequestFrom(ctx) != nil
		return context.WithValue(ctx, userKey{}, "ada"), nil
	}))
	c.init("")
	if !sawRequest {
		t.Fatal("the ConnectFunc itself was not given the upgrade request")
	}
	c.subscribe("1", `{ seen }`)
	if got := c.recv(); string(got.Payload) != `{"data":{"seen":"ada: nothing"}}` {
		t.Fatalf("frame = %s: what the hook put on the context must reach a resolver, and the upgrade request must not", got.Payload)
	}
}

// An oversized client message must end the connection rather than be
// buffered. gqlfiber had this test and gqlws did not, so nothing held the
// option to the socket here: with the limit not applied the message is read
// whole and the handshake succeeds.
func TestReadLimitClosesAnOversizedMessage(t *testing.T) {
	_, e := newTestExecutor(t)
	c := dial(t, e, gqlws.WithReadLimit(128))
	c.send(frame{Type: "connection_init", Payload: json.RawMessage(`{"pad":"` + strings.Repeat("x", 512) + `"}`)})
	if got := c.recvErr(); got != websocket.StatusMessageTooBig {
		t.Fatalf("close code = %d, want %d", got, websocket.StatusMessageTooBig)
	}
}
