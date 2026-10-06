package gqlfiber

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
)

// deadlineListener counts the write deadlines set on the connections it
// accepts. The connection is where this package's SSE keeps its deadline, and
// nothing else sets one: fasthttp does only under a WriteTimeout, which these
// servers do not configure.
type deadlineListener struct {
	net.Listener
	sets *atomic.Int64
}

func (l deadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return deadlineConn{Conn: c, sets: l.sets}, nil
}

type deadlineConn struct {
	net.Conn
	sets *atomic.Int64
}

func (c deadlineConn) SetWriteDeadline(at time.Time) error {
	c.sets.Add(1)
	return c.Conn.SetWriteDeadline(at)
}

// gqlsse's test of the same name, counted where Fiber keeps the deadline. An
// event is five writes, and each used to set it again; an event written with
// none is one a stalled reader parks the stream in for good.
func TestEachSSEEventIsWrittenUnderOneDeadline(t *testing.T) {
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
	app := fiber.New()
	app.Post("/graphql", SSE(graphql.NewExecutor(s), WithKeepAlive(0)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var sets atomic.Int64
	go func() {
		_ = app.Listener(deadlineListener{Listener: ln, sets: &sets}, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() { _ = app.ShutdownWithTimeout(2 * time.Second) })

	// The response is read to its end, which the server reaches only after
	// the stream writer has returned: the count is final by then.
	ask := func(t *testing.T, query string) (string, int64) {
		t.Helper()
		sets.Store(0)
		req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/graphql", strings.NewReader(`{"query":"`+query+`"}`))
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
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		return string(body), sets.Load()
	}

	t.Run("a subscription", func(t *testing.T) {
		go func() {
			ticks <- 1
			ticks <- 2
			close(ticks)
		}()
		body, n := ask(t, "subscription { tick }")
		if strings.Count(body, "event: next") != 2 || !strings.HasSuffix(body, "event: complete\ndata:\n\n") {
			t.Fatalf("stream = %q", body)
		}
		// One for each of the two events, one for the complete, and the one
		// left armed for what fasthttp writes after the stream writer returns.
		if n != 4 {
			t.Errorf("the deadline was set %d times, want 4", n)
		}
	})

	t.Run("a query", func(t *testing.T) {
		body, n := ask(t, "{ ping }")
		if !strings.Contains(body, `"ping":"pong"`) {
			t.Fatalf("stream = %q", body)
		}
		// One for the next and the complete together, one for their flush and
		// the one left armed.
		if n != 3 {
			t.Errorf("the deadline was set %d times, want 3", n)
		}
	})
}
