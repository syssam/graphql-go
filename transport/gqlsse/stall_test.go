package gqlsse_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

// A subscriber that stays connected and stops reading parks the handler in a
// socket write, where it sees neither the drain nor its cancelled context. It
// then held its stream past drain.Shutdown and http.Server.Shutdown both, the
// very thing the drain exists to prevent, until the peer or the kernel gave
// up. The write timeout is what ends it.
func TestAStalledReaderIsEndedByTheWriteTimeout(t *testing.T) {
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
	srv := httptest.NewServer(gqlsse.New(graphql.NewExecutor(s),
		gqlsse.WithDrain(d), gqlsse.WithWriteTimeout(200*time.Millisecond)))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	body := `{"query":"subscription { blob }"}`
	_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAccept: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	if err != nil {
		t.Fatalf("write request: %v", err)
	}
	// The status line proves the stream opened; nothing is read after it, so
	// the socket buffers fill and the handler's next write blocks.
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("status line = %q, %v", status, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for d.Active() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d stream still registered with the drain: the handler is parked in a write no deadline bounds", d.Active())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
