package gqlfiber

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// TestSSEWithMaxStreamAgeEndsStreamWithoutComplete proves the option reaches
// the stream loop: a source that never produces an event is ended by the age
// timer alone, and it ends the way a drain does -- no complete event, so the
// client reconnects instead of treating the subscription as finished for
// good.
func TestSSEWithMaxStreamAgeEndsStreamWithoutComplete(t *testing.T) {
	src := newIdleSource()
	app := fiber.New()
	app.Post("/graphql", SSE(idleSSEExecutor(t, src),
		WithKeepAlive(time.Hour), WithMaxStreamAge(100*time.Millisecond)))
	base := startFiber(t, app)

	resp, err := streamClient(t).Do(sseSubscribeRequest(t, context.Background(), base))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	type readResult struct {
		b   []byte
		err error
	}
	rest := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(resp.Body)
		rest <- readResult{b, err}
	}()

	select {
	case r := <-rest:
		if r.err != nil {
			t.Fatalf("reading the stream to its end: %v (read %q)", r.err, r.b)
		}
		if strings.Contains(string(r.b), "event: complete") {
			t.Fatalf("stream carried a complete event: %q", r.b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end within 2s of a 100ms WithMaxStreamAge")
	}

	select {
	case <-src.released:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d subscription(s) still open after the stream aged out", src.live.Load())
	}
}

// TestSSEWithMaxStreamAgeLeavesSingleResultAlone: a query or mutation is one
// next then complete before the age timer could ever matter, so the option
// must not touch that path.
func TestSSEWithMaxStreamAgeLeavesSingleResultAlone(t *testing.T) {
	app := fiber.New()
	app.Post("/graphql", SSE(newTestExecutor(t), WithMaxStreamAge(100*time.Millisecond)))
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	const want = "event: next\ndata: {\"data\":{\"hello\":\"world\"}}\n\nevent: complete\ndata:\n\n"
	if got := string(body); got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}
