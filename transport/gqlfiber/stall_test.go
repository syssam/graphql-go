package gqlfiber

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
)

// gqlsse's stall test, against the Fiber stream: a subscriber that stays
// connected and stops reading parks the stream in a socket write, where it
// sees neither the drain nor its context.
func TestAStalledSSEReaderIsEndedByTheWriteTimeout(t *testing.T) {
	blob := strings.Repeat("x", 256<<10)
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { ping: String! }
		type Subscription { blob: String! }
	`),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Subscription(graphql.Subscribe("blob", func(ctx context.Context) (<-chan string, error) {
			out := make(chan string)
			go func() {
				defer close(out)
				for {
					select {
					case out <- blob:
					case <-ctx.Done():
						return
					}
				}
			}()
			return out, nil
		})),
	)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	d := drain.New()
	app := fiber.New()
	app.All("/graphql", SSE(graphql.NewExecutor(s),
		WithDrain(d), WithWriteTimeout(200*time.Millisecond)))
	base := startFiber(t, app)

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	body := `{"query":"subscription { blob }"}`
	_, err = fmt.Fprintf(conn, "POST /graphql HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAccept: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if err != nil {
		t.Fatalf("write request: %v", err)
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("status line = %q, %v", status, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for d.Active() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d stream still registered with the drain: the stream is parked in a write no deadline bounds", d.Active())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The write deadline lives on the connection, and fasthttp does not reset it
// between responses unless a WriteTimeout is configured. A stream that ended
// without complete -- by its age limit here -- left its last deadline armed on
// a connection the client was free to reuse, and the next response written on
// it after that instant failed.
func TestAnEndedSSEStreamLeavesNoDeadlineForTheNextRequest(t *testing.T) {
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
	app.All("/graphql", SSE(graphql.NewExecutor(s),
		WithWriteTimeout(100*time.Millisecond), WithKeepAlive(0), WithMaxStreamAge(300*time.Millisecond)))
	base := startFiber(t, app)

	// One connection at most, so the second request has to take the first's
	// if the server left it open.
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}}
	post := func(query string) (*http.Response, []byte, error) {
		req, _ := http.NewRequest(http.MethodPost, base+"/graphql", strings.NewReader(`{"query":`+strconv.Quote(query)+`}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return resp, body, err
	}

	go func() { ticks <- 1 }()
	if _, body, err := post(`subscription { tick }`); err != nil || !strings.Contains(string(body), `"tick":1`) {
		t.Fatalf("stream: body %q, err %v", body, err)
	}
	time.Sleep(300 * time.Millisecond)
	_, body, err := post(`{ ping }`)
	if err != nil || !strings.Contains(string(body), `"ping":"pong"`) {
		t.Fatalf("the request after the stream failed: body %q, err %v", body, err)
	}
}
