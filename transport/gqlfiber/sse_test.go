package gqlfiber

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
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

	resp, err := http.DefaultClient.Do(req)
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

	resp, err := http.DefaultClient.Do(req)
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

	resp, err := http.DefaultClient.Do(req)
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
