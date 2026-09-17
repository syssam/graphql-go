package gqlfiber

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
)

// shutdownResult is what a Shutdown run in the background reports.
type shutdownResult struct {
	err error
	dur time.Duration
}

func shutdownAsync(d *drain.Drain) <-chan shutdownResult {
	done := make(chan shutdownResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := d.Shutdown(ctx)
		done <- shutdownResult{err, time.Since(start)}
	}()
	return done
}

// requirePromptShutdown fails unless Shutdown returned nil well inside its
// own 5s deadline: returning at the deadline would mean the drain gave up on
// a connection rather than the connection leaving.
func requirePromptShutdown(t *testing.T, done <-chan shutdownResult) {
	t.Helper()
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

// An idle subscription produces nothing and its source honours only its
// context, so the drain's Closing reaching the protocol is the only thing
// that can end this connection with 1001.
func TestWSDrainClosesWithGoingAway(t *testing.T) {
	src := newIdleWSSource()
	d := drain.New()
	c := dialWS(t, idleExecutor(t, src), WithDrain(d))
	c.init()

	c.subscribe("1", `subscription { ticks }`)
	waitRegistered(t, src, 1)

	done := shutdownAsync(d)

	if got := c.recvClose(); got != coderws.StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, coderws.StatusGoingAway)
	}
	requirePromptShutdown(t, done)

	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still registered after the drain", src.live.Load())
	}
}

// blockingExecutor serves a query that reports when it starts and then
// returns only once its context is cancelled.
func blockingExecutor(t *testing.T) (<-chan struct{}, *graphql.Executor) {
	t.Helper()
	started := make(chan struct{}, 1)
	s, err := graphql.NewSchema(graphql.SDL(`type Query { block: String! }`),
		graphql.Query(graphql.Resolve("block", func(ctx context.Context, _ graphql.Root) (string, error) {
			started <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return started, graphql.NewExecutor(s)
}

// A query that ignores Closing keeps the connection's drain waiting, so
// Shutdown can only return DeadlineExceeded if the connection was registered,
// and the 1001 can only arrive if giving up reaches the context the protocol
// runs under. Closing alone produces neither.
func TestWSDrainWaitsForConnectionThenGivesUp(t *testing.T) {
	started, exec := blockingExecutor(t)
	d := drain.New()
	c := dialWS(t, exec, WithDrain(d))
	c.init()

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

	if got := c.recvClose(); got != coderws.StatusGoingAway {
		t.Fatalf("close code = %d, want %d", got, coderws.StatusGoingAway)
	}
}

// Once draining has begun an upgrade is refused with a status before the
// socket is hijacked, the way gqlws refuses it.
func TestWSDrainRefusesUpgrade(t *testing.T) {
	d := drain.New()
	app := fiber.New()
	app.Get("/graphql", WS(newTestExecutor(t), WithDrain(d)))
	base := startFiber(t, app)

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, resp, err := coderws.Dial(ctx, wsURL(base+"/graphql"),
		&coderws.DialOptions{Subprotocols: []string{Subprotocol}})
	if err == nil {
		defer ws.CloseNow()
		_, _, rerr := ws.Read(ctx)
		t.Fatalf("dial succeeded while the drain was shutting down; the connection then ended with close code %d (%v)",
			coderws.CloseStatus(rerr), rerr)
	}
	if resp == nil {
		t.Fatalf("no response returned for the refused upgrade: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, http.StatusServiceUnavailable, err)
	}
}

func sseSubscribeRequest(t *testing.T, ctx context.Context, base string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/graphql",
		strings.NewReader(`{"query":"subscription{ticks}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req
}

func idleSSEExecutor(t *testing.T, src *idleSource) *graphql.Executor {
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

// A drained stream ends without complete, so a client reconnects rather than
// treating the subscription as finished. The source is idle and keep-alive is
// far beyond the test's deadlines, so nothing but the drain can end it.
func TestSSEDrainEndsStreamWithoutComplete(t *testing.T) {
	src := newIdleSource()
	d := drain.New()
	app := fiber.New()
	app.Post("/graphql", SSE(idleSSEExecutor(t, src), WithKeepAlive(time.Hour), WithDrain(d)))
	base := startFiber(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := streamClient(t).Do(sseSubscribeRequest(t, ctx, base))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	done := shutdownAsync(d)

	type readResult struct {
		b   []byte
		err error
	}
	rest := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(resp.Body)
		rest <- readResult{b, err}
	}()
	var read []byte
	select {
	case r := <-rest:
		if r.err != nil {
			t.Fatalf("reading the stream to its end: %v (read %q)", r.err, r.b)
		}
		read = r.b
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading the stream to its end")
	}
	if strings.Contains(string(read), "event: complete") {
		t.Fatalf("stream carried a complete event: %q", read)
	}

	requirePromptShutdown(t, done)

	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still open after the drain", src.live.Load())
	}
}

func TestSSEDrainRefusesNewSubscription(t *testing.T) {
	src := newIdleSource()
	d := drain.New()
	app := fiber.New()
	app.Post("/graphql", SSE(idleSSEExecutor(t, src), WithKeepAlive(time.Hour), WithDrain(d)))
	base := startFiber(t, app)

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	resp, err := streamClient(t).Do(sseSubscribeRequest(t, context.Background(), base))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := src.live.Load(); got != 0 {
		t.Fatalf("live subscriptions = %d, want 0: the refused request still subscribed", got)
	}
}

// A subscription whose opener fails is refused before any stream begins, and
// that path must leave the drain too, or Shutdown waits out its deadline for a
// request that already ended.
func TestSSESubscribeErrorLeavesDrain(t *testing.T) {
	const sdl = `
type Query { hello: String! }
type Subscription { ticks: Int! }
`
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("ticks", func(context.Context) (<-chan int, error) {
			return nil, errors.New("no ticks today")
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	d := drain.New()
	app := fiber.New()
	app.Post("/graphql", SSE(graphql.NewExecutor(s), WithKeepAlive(time.Hour), WithDrain(d)))
	base := startFiber(t, app)

	resp, err := streamClient(t).Do(sseSubscribeRequest(t, context.Background(), base))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if dur := time.Since(start); dur > time.Second {
		t.Fatalf("Shutdown took %v, want under 1s", dur)
	}
}

// The context OnConnect returns can end on its own, as an expiring token
// would. fasthttp's Read ignores its context, so only a close reaches the
// parked read; without one the client keeps an open, silent socket.
func TestWSCancelledConnectContextCloses(t *testing.T) {
	session, expire := context.WithCancel(context.Background())
	defer expire()
	c := dialWS(t, newTestExecutor(t), WithOnConnect(func(ctx context.Context, _ []byte) (context.Context, error) {
		ctx, cancel := context.WithCancel(ctx)
		context.AfterFunc(session, cancel)
		return ctx, nil
	}))
	c.init()

	expire()
	if got := c.recvClose(); got != coderws.StatusGoingAway {
		t.Fatalf("close code = %d after the connection context ended, want %d", got, coderws.StatusGoingAway)
	}
}

// Fiber's own shutdown waits for open requests, and an SSE subscription is one
// that only the drain ends, so the two must be run together and both finish
// promptly. The app's real listener is used so ShutdownWithContext is the one
// a deployment calls.
func TestSSEFiberShutdownFinishesWithDrain(t *testing.T) {
	src := newIdleSource()
	d := drain.New()
	app := fiber.New()
	app.Post("/graphql", SSE(idleSSEExecutor(t, src), WithKeepAlive(time.Hour), WithDrain(d)))
	base := startFiber(t, app)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	resp, err := streamClient(t).Do(sseSubscribeRequest(t, reqCtx, base))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	go func() { _, _ = io.Copy(io.Discard, resp.Body) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	drained, stopped := make(chan error, 1), make(chan error, 1)
	go func() { drained <- d.Shutdown(ctx) }()
	go func() { stopped <- app.ShutdownWithContext(ctx) }()
	for _, r := range []struct {
		name string
		ch   <-chan error
	}{{"drain.Shutdown", drained}, {"app.ShutdownWithContext", stopped}} {
		select {
		case err := <-r.ch:
			if err != nil {
				t.Fatalf("%s = %v, want nil", r.name, err)
			}
		case <-time.After(6 * time.Second):
			t.Fatalf("%s did not return", r.name)
		}
	}
	if dur := time.Since(start); dur > 2*time.Second {
		t.Fatalf("shutdown took %v, want under 2s", dur)
	}
}
