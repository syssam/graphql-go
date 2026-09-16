package gqlfiber

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
)

// idleSource opens subscription streams that never produce an event, so the
// operation context being cancelled is the only thing that can end one. It
// counts the streams still holding their goroutine: a leak then names itself
// here instead of surfacing later as a stray goroutine somewhere else.
type idleSource struct {
	live     atomic.Int64
	released chan struct{}
}

func newIdleSource() *idleSource { return &idleSource{released: make(chan struct{})} }

func (s *idleSource) stream(ctx context.Context) (<-chan int, error) {
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

// streamClient bounds the wait for a response head without bounding the read
// of the stream behind it.
//
// Every other wait in this file is bounded, and this one has to be too: these
// tests subscribe to sources that produce nothing before Do returns, so Do
// completes only because the handler flushes the response head immediately.
// Lose that and an unbounded Do would hang until the whole run's timeout
// dumped every goroutine in the process, instead of failing in five seconds
// and naming the cause.
func streamClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{ResponseHeaderTimeout: 5 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// A dropped SSE client must cancel the operation context. Fiber's context
// never cancels and fasthttp reports no disconnect, so the only evidence the
// stream gets is a write of its own failing -- which on an idle subscription
// means the keep-alive tick. The client here leaves with nothing pending, so
// a handler that never cancels holds the subscription for the process
// lifetime; one that does releases it on the first failed flush.
func TestSSECancelsOnIdleClientDisconnect(t *testing.T) {
	src := newIdleSource()
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

	app := fiber.New()
	app.Post("/graphql", SSE(graphql.NewExecutor(s), WithKeepAlive(20*time.Millisecond)))
	base := startFiber(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/graphql",
		strings.NewReader(`{"query":"subscription{ticks}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := src.live.Load(); got != 1 {
		resp.Body.Close()
		t.Fatalf("live subscriptions = %d, want 1", got)
	}

	// Drop the client with nothing pending.
	cancel()
	resp.Body.Close()

	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still open 5s after the client disconnected", src.live.Load())
	}
}

func TestSSEStreamsAQuery(t *testing.T) {
	app := fiber.New()
	app.Post("/graphql", SSE(newTestExecutor(t)))
	base := startFiber(t, app)

	req, err := http.NewRequest(http.MethodPost, base+"/graphql", strings.NewReader(`{"query":"{hello}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != MediaTypeEventStream {
		t.Fatalf("Content-Type = %q, want %q", got, MediaTypeEventStream)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	// Pinned byte for byte against gqlsse's writeNext and writeComplete: a
	// client must not be able to tell which transport served it.
	const want = "event: next\ndata: {\"data\":{\"hello\":\"world\"}}\n\nevent: complete\ndata:\n\n"
	if got := string(body); got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

// A keep-alive of zero stays disabled rather than being clamped, so that the
// option means the same thing here as on gqlsse. But on Fiber it removes the
// only way an idle stream can learn its client has gone, which is too much to
// leave to a doc comment.
func TestSSEWarnsWhenKeepAliveIsDisabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	SSE(newTestExecutor(t), WithKeepAlive(0), WithLogger(logger))
	if !strings.Contains(buf.String(), "keep-alive is disabled") {
		t.Fatalf("disabling keep-alive logged %q, want a warning about it", buf.String())
	}

	buf.Reset()
	SSE(newTestExecutor(t), WithLogger(logger))
	if buf.Len() != 0 {
		t.Fatalf("the default keep-alive warned: %q", buf.String())
	}
}

// panicOnFlushWarning is a logger that panics the first time the stream
// reports a failed flush, and records everything else.
//
// Injecting through the logger is not arbitrary: it is the only reach a test
// has into the stream-writer goroutine. Everything else that runs there is
// fasthttp's own buffer and the engine's serialisation, and a panicking
// resolver does not help -- the executor runs bindings on its own pump
// goroutine, so its panic never reaches this closure. A slog handler that
// panics is also a real hazard in its own right.
type panicOnFlushWarning struct {
	mu       sync.Mutex
	injected bool
	messages []string
}

func (h *panicOnFlushWarning) Enabled(context.Context, slog.Level) bool { return true }

func (h *panicOnFlushWarning) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.injected && strings.Contains(r.Message, "flushing stream") {
		h.injected = true
		panic("gqlfiber test: panic injected into the stream writer")
	}
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *panicOnFlushWarning) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *panicOnFlushWarning) WithGroup(string) slog.Handler      { return h }

func (h *panicOnFlushWarning) didInject() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.injected
}

func (h *panicOnFlushWarning) logged(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// A panic on the stream-writer goroutine must cost one connection, not the
// process, and must not strand the subscription behind it.
//
// fasthttp runs that closure on a bare goroutine with no recover of its own,
// so without the recover this test does not fail -- it takes the whole test
// binary down with it. With the recover but without the cleanup defer running
// during unwinding, the process survives and the subscription is held
// forever, which is what the registration count below catches.
func TestSSERecoversPanicAndReleasesSubscription(t *testing.T) {
	src := newIdleSource()
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

	logs := &panicOnFlushWarning{}
	app := fiber.New()
	app.Post("/graphql", SSE(graphql.NewExecutor(s),
		WithKeepAlive(20*time.Millisecond), WithLogger(slog.New(logs))))
	base := startFiber(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/graphql",
		strings.NewReader(`{"query":"subscription{ticks}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := src.live.Load(); got != 1 {
		resp.Body.Close()
		t.Fatalf("live subscriptions = %d, want 1", got)
	}

	// Drop the client, so the keep-alive tick fails its flush and the panic
	// goes off inside the stream writer.
	cancel()
	resp.Body.Close()

	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still held 5s after a panic on the stream writer", src.live.Load())
	}

	// Asserted after the wait, because a run where the flush never failed
	// would release the subscription for an unrelated reason and prove
	// nothing about the panic path.
	if !logs.didInject() {
		t.Fatal("no panic was ever injected -- the flush never failed, so this test proved nothing")
	}
	// Polled rather than read once: the recover and the release run in the
	// same unwinding, and which lands first is a detail of how the two
	// deferred funcs are ordered. Asserting it without a bound would make
	// this test pass or fail on scheduling.
	deadline := time.Now().Add(2 * time.Second)
	for !logs.logged("panic while streaming") {
		if time.Now().After(deadline) {
			t.Fatal("the panic was not logged; nothing shows recover ran rather than the process dying")
		}
		time.Sleep(time.Millisecond)
	}
}

// newPacedExecutor builds a subscription driven by an unbuffered channel, so
// a send completes only once the executor's pump has taken it and the test
// can release events one at a time.
func newPacedExecutor(t *testing.T) (chan string, *graphql.Executor) {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Subscription { ticks: String! }
`
	ch := make(chan string)
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("ticks", func(context.Context) (<-chan string, error) {
			return ch, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return ch, graphql.NewExecutor(s)
}

// Events must reach the client as they happen. The test publishes the second
// event only after reading the first off a still-open stream: a version that
// left everything in fasthttp's stream buffer until the handler returned
// would hang here rather than pass, which reading to EOF would not catch.
func TestSSEStreamsSubscriptionEventsIncrementally(t *testing.T) {
	ch, exec := newPacedExecutor(t)

	app := fiber.New()
	app.Post("/graphql", SSE(exec))
	base := startFiber(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/graphql",
		strings.NewReader(`{"query":"subscription{ticks}"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				lines <- sc.Text()
				return
			}
		}
		lines <- ""
	}()

	select {
	case ch <- "one":
	case <-time.After(5 * time.Second):
		t.Fatal("timed out publishing the first event")
	}

	var first string
	select {
	case first = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first event -- the stream is not being flushed")
	}
	if !strings.Contains(first, `"ticks":"one"`) {
		t.Fatalf("first event = %q, want it to carry %q", first, "one")
	}

	// The client has read exactly one event. Publishing the second only now
	// proves delivery is incremental, not merely eventual.
	select {
	case ch <- "two":
	case <-time.After(5 * time.Second):
		t.Fatal("timed out publishing the second event")
	}
	close(ch)
}
