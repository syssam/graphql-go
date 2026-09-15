package gqlfiber

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
)

// wsFrame is one graphql-transport-ws message.
type wsFrame struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// wsClient is a minimal graphql-transport-ws client, mirroring the one in
// transport/gqlws so that a difference between the two drivers shows up as a
// difference in the assertions rather than in the client.
type wsClient struct {
	t  *testing.T
	ws *coderws.Conn
}

func wsURL(base string) string { return "ws" + strings.TrimPrefix(base, "http") }

// dialWS starts an app serving WS at /graphql and connects to it.
func dialWS(t *testing.T, exec *graphql.Executor, opts ...Option) *wsClient {
	t.Helper()
	app := fiber.New()
	app.Get("/graphql", WS(exec, opts...))
	return dialWSAt(t, startFiber(t, app)+"/graphql", nil)
}

func dialWSAt(t *testing.T, url string, header http.Header) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ws, _, err := coderws.Dial(ctx, wsURL(url), &coderws.DialOptions{
		Subprotocols: []string{Subprotocol},
		HTTPHeader:   header,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &wsClient{t: t, ws: ws}
}

func (c *wsClient) send(f wsFrame) {
	c.t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, coderws.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *wsClient) recv() wsFrame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var f wsFrame
	if err := json.Unmarshal(data, &f); err != nil {
		c.t.Fatalf("decode %s: %v", data, err)
	}
	return f
}

// recvClose drains until the connection ends and returns the close code.
func (c *wsClient) recvClose() coderws.StatusCode {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return coderws.CloseStatus(err)
		}
	}
}

func (c *wsClient) init() {
	c.t.Helper()
	c.send(wsFrame{Type: "connection_init"})
	if got := c.recv(); got.Type != "connection_ack" {
		c.t.Fatalf("handshake returned %+v, want connection_ack", got)
	}
}

func (c *wsClient) subscribe(id, query string) {
	c.t.Helper()
	b, _ := json.Marshal(query)
	c.send(wsFrame{ID: id, Type: "subscribe", Payload: json.RawMessage(`{"query":` + string(b) + `}`)})
}

func TestWSQueryIsNextThenComplete(t *testing.T) {
	c := dialWS(t, newTestExecutor(t))
	c.init()

	c.subscribe("1", `{ hello }`)
	if got := c.recv(); got.Type != "next" || got.ID != "1" || string(got.Payload) != `{"data":{"hello":"world"}}` {
		t.Fatalf("first frame = %+v", got)
	}
	if got := c.recv(); got.Type != "complete" || got.ID != "1" {
		t.Fatalf("second frame = %+v", got)
	}
}

func TestWSMutationIsNextThenComplete(t *testing.T) {
	c := dialWS(t, newTestExecutor(t))
	c.init()

	c.subscribe("m", `mutation { bump }`)
	if got := c.recv(); got.Type != "next" || string(got.Payload) != `{"data":{"bump":"bumped"}}` {
		t.Fatalf("first frame = %+v", got)
	}
	if got := c.recv(); got.Type != "complete" {
		t.Fatalf("second frame = %+v", got)
	}
}

// countdownExecutor streams a bounded subscription so that a test can watch
// several next frames followed by complete.
func countdownExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Subscription { countdown: Int! }
`
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("countdown", func(ctx context.Context) (<-chan int, error) {
			ch := make(chan int)
			go func() {
				defer close(ch)
				for i := 3; i > 0; i-- {
					select {
					case ch <- i:
					case <-ctx.Done():
						return
					}
				}
			}()
			return ch, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}

func TestWSSubscriptionStreamsThenCompletes(t *testing.T) {
	c := dialWS(t, countdownExecutor(t))
	c.init()

	c.subscribe("s", `subscription { countdown }`)
	for i := 3; i > 0; i-- {
		got := c.recv()
		want := `{"data":{"countdown":` + string(rune('0'+i)) + `}}`
		if got.Type != "next" || got.ID != "s" || string(got.Payload) != want {
			t.Fatalf("frame = %+v, want next %s", got, want)
		}
	}
	if got := c.recv(); got.Type != "complete" || got.ID != "s" {
		t.Fatalf("terminal frame = %+v, want complete", got)
	}
}

// A connection that never initialises must be closed with 4408. It also pins
// that InitTimeout reaches gqlwsproto at all: left unset it is zero, and
// time.AfterFunc(0) closes every connection before its first message.
func TestWSInitTimeout(t *testing.T) {
	c := dialWS(t, newTestExecutor(t), WithInitTimeout(50*time.Millisecond))
	if got := c.recvClose(); got != 4408 {
		t.Fatalf("close code = %d, want 4408", got)
	}
}

// Pings are the only thing an idle connection sends, so an unset PingInterval
// is invisible except here.
func TestWSSendsPings(t *testing.T) {
	c := dialWS(t, newTestExecutor(t), WithPingInterval(20*time.Millisecond))
	c.init()
	if got := c.recv(); got.Type != "ping" {
		t.Fatalf("frame = %+v, want ping", got)
	}
}

func TestWSMaxSubscriptions(t *testing.T) {
	src := newIdleWSSource()
	c := dialWS(t, idleExecutor(t, src), WithMaxSubscriptions(1))
	c.init()

	// A subscription that never ends holds the only slot.
	c.subscribe("a", `subscription { ticks }`)
	waitRegistered(t, src, 1)

	c.subscribe("b", `subscription { ticks }`)
	got := c.recv()
	if got.ID != "b" || got.Type != "error" {
		t.Fatalf("second subscribe answered with %+v, want an error for b", got)
	}
	if !strings.Contains(string(got.Payload), "at most 1 operations") {
		t.Fatalf("error payload = %s", got.Payload)
	}
}

func TestWSOnConnectSeesUpgradeRequest(t *testing.T) {
	const sdl = `type Query { whoami: String! }`
	type userKey struct{}
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Resolve("whoami", func(ctx context.Context, _ graphql.Root) (string, error) {
			name, _ := ctx.Value(userKey{}).(string)
			if name == "" {
				name = "anonymous"
			}
			return name, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}

	app := fiber.New()
	app.Get("/graphql", WS(graphql.NewExecutor(s), WithOnConnect(
		func(ctx context.Context, _ []byte) (context.Context, error) {
			conn := ConnFrom(ctx)
			if conn == nil {
				t.Error("no connection on the ConnectFunc context")
				return ctx, nil
			}
			return context.WithValue(ctx, userKey{}, conn.Headers("X-User")), nil
		})))
	base := startFiber(t, app)

	c := dialWSAt(t, base+"/graphql", http.Header{"X-User": {"ada"}})
	c.init()
	c.subscribe("1", `{ whoami }`)
	if got := c.recv(); string(got.Payload) != `{"data":{"whoami":"ada"}}` {
		t.Fatalf("payload = %s", got.Payload)
	}
}

// An oversized client message must end the connection rather than be
// buffered. Without SetReadLimit the library's own default of no limit
// applies and a client can make the server allocate without bound.
func TestWSReadLimit(t *testing.T) {
	c := dialWS(t, newTestExecutor(t), WithReadLimit(128))
	c.send(wsFrame{Type: "connection_init", Payload: json.RawMessage(
		`{"pad":"` + strings.Repeat("x", 512) + `"}`)})
	if got := c.recvClose(); got != coderws.StatusMessageTooBig {
		t.Fatalf("close code = %d, want %d", got, coderws.StatusMessageTooBig)
	}
}

func TestWSRejectsNonUpgradeRequest(t *testing.T) {
	app := fiber.New()
	app.Get("/graphql", WS(newTestExecutor(t)))
	base := startFiber(t, app)

	resp, err := http.Get(base + "/graphql")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", resp.StatusCode)
	}
}

func TestWSRejectsMissingSubprotocol(t *testing.T) {
	app := fiber.New()
	app.Get("/graphql", WS(newTestExecutor(t)))
	base := startFiber(t, app)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := coderws.Dial(ctx, wsURL(base+"/graphql"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	if _, _, err := ws.Read(ctx); coderws.CloseStatus(err) != 4406 {
		t.Fatalf("close status = %d (%v), want 4406", coderws.CloseStatus(err), err)
	}
}

func originDial(t *testing.T, base, origin string, opts ...Option) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := coderws.Dial(ctx, wsURL(base), &coderws.DialOptions{
		Subprotocols: []string{Subprotocol},
		HTTPHeader:   http.Header{"Origin": {origin}},
	})
	if err == nil {
		ws.CloseNow()
	}
	return err
}

func TestWSOriginCheck(t *testing.T) {
	newApp := func(opts ...Option) string {
		app := fiber.New()
		app.Get("/graphql", WS(newTestExecutor(t), opts...))
		return startFiber(t, app) + "/graphql"
	}

	t.Run("cross origin is refused by default", func(t *testing.T) {
		url := newApp()
		if err := originDial(t, url, "http://evil.example"); err == nil {
			t.Fatal("dial succeeded, want the upgrade refused")
		}
	})

	t.Run("same origin needs no configuration", func(t *testing.T) {
		url := newApp()
		host := strings.TrimPrefix(url, "http://")
		host = strings.TrimSuffix(host, "/graphql")
		if err := originDial(t, url, "http://"+host); err != nil {
			t.Fatalf("same-origin dial: %v", err)
		}
	})

	t.Run("a configured pattern is authorized", func(t *testing.T) {
		url := newApp(WithOriginPatterns("*.example"))
		if err := originDial(t, url, "http://app.example"); err != nil {
			t.Fatalf("pattern dial: %v", err)
		}
	})

	t.Run("the check can be disabled", func(t *testing.T) {
		url := newApp(WithInsecureSkipOriginCheck())
		if err := originDial(t, url, "http://evil.example"); err != nil {
			t.Fatalf("insecure dial: %v", err)
		}
	})

	t.Run("a client sending no origin is not a browser", func(t *testing.T) {
		url := newApp()
		c := dialWSAt(t, url, nil)
		c.init()
	})
}

// idleWSSource opens subscription streams that never produce an event, so the
// operation context being cancelled is the only thing that can end one. It
// counts the streams still holding their goroutine: a leak then names itself
// here instead of surfacing later as a stray goroutine somewhere else.
type idleWSSource struct {
	live     atomic.Int64
	released chan struct{}
}

func newIdleWSSource() *idleWSSource { return &idleWSSource{released: make(chan struct{})} }

func (s *idleWSSource) stream(ctx context.Context) (<-chan int, error) {
	s.live.Add(1)
	ch := make(chan int)
	go func() {
		defer close(ch)
		<-ctx.Done()
		if s.live.Add(-1) == 0 {
			close(s.released)
		}
	}()
	return ch, nil
}

// waitRegistered blocks until n streams have reached the source. A subscribe
// is asynchronous, so nothing else says the operation is actually running.
func waitRegistered(t *testing.T, src *idleWSSource, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for src.live.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d subscription(s) reached the source", src.live.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func idleExecutor(t *testing.T, src *idleWSSource) *graphql.Executor {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Subscription { ticks: Int! }
`
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("ticks", src.stream)),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}

// A client that leaves with nothing pending must still release its
// subscriptions. This is the case that discriminates: with an event in
// flight, the write failing reclaims the operation whether or not
// cancellation propagates at all, so only an idle subscription proves that
// the read ending tears the connection down.
func TestWSCancelsIdleSubscriptionOnClientClose(t *testing.T) {
	src := newIdleWSSource()
	c := dialWS(t, idleExecutor(t, src))
	c.init()

	c.subscribe("1", `subscription { ticks }`)
	waitRegistered(t, src, 1)

	c.ws.CloseNow()

	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still registered 5s after the client left", src.live.Load())
	}
}

// blastExecutor floods a subscription with payloads of the given size, so
// that a peer which stops reading backs the connection up into a blocking
// write, and so that a peer which keeps reading keeps writes in flight.
func blastExecutor(t *testing.T, src *idleWSSource, size int) *graphql.Executor {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Subscription { blast: String! }
`
	payload := strings.Repeat("x", size)
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("blast", func(ctx context.Context) (<-chan string, error) {
			src.live.Add(1)
			ch := make(chan string)
			go func() {
				defer close(ch)
				defer func() {
					if src.live.Add(-1) == 0 {
						close(src.released)
					}
				}()
				for {
					select {
					case ch <- payload:
					case <-ctx.Done():
						return
					}
				}
			}()
			return ch, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}

// A peer that stops reading must not hold the connection forever.
// fasthttp/websocket's WriteMessage takes no context, so the write deadline
// is the only bound there is; without it the stalled write keeps the
// protocol's write lock and every subscription on the connection with it.
func TestWSWriteDeadlineReleasesAStalledPeer(t *testing.T) {
	src := newIdleWSSource()
	c := dialWS(t, blastExecutor(t, src, 32<<10), WithWriteTimeout(200*time.Millisecond))
	c.ws.SetReadLimit(1 << 20)
	c.init()

	c.subscribe("1", `subscription { blast }`)
	// Read one frame to prove the stream is running, then stop reading and
	// let the socket back up.
	if got := c.recv(); got.Type != "next" {
		t.Fatalf("frame = %+v, want next", got)
	}

	select {
	case <-src.released:
	case <-time.After(15 * time.Second):
		t.Fatalf("%d subscription(s) still registered after the peer stopped reading", src.live.Load())
	}

	// The connection must go too. Releasing the operation is not enough: the
	// read loop is parked in ReadMessage, which no context reaches, so unless
	// the failed write closes the socket the connection and its goroutine are
	// held for the lifetime of the process. Draining to the error is how a
	// client sees that; the backlog already in its receive buffer comes first.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the connection was still open after its write failed")
			}
			return
		}
	}
}

// gqlwsproto guarantees only that writes do not overlap each other: a close
// may arrive from the read loop -- here, from rejecting a second
// connection_init -- while operations are still streaming. fasthttp/websocket
// shares one write buffer between a data frame and a close frame, so an
// unserialized close both corrupts the stream and races; under -race the
// detector reports it.
func TestWSCloseSerializesAgainstWrites(t *testing.T) {
	src := newIdleWSSource()
	c := dialWS(t, blastExecutor(t, src, 256))
	c.init()
	c.subscribe("1", `subscription { blast }`)

	// Keep reading so that the server always has a write in flight.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for {
			if _, _, err := c.ws.Read(ctx); err != nil {
				return
			}
		}
	}()
	waitRegistered(t, src, 1)

	c.send(wsFrame{Type: "connection_init"})

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the connection stayed open after a second connection_init")
	}
	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still registered after the connection closed", src.live.Load())
	}
}
