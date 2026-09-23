package gqlws_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// A peer that stays connected but stops reading must not hold its connection
// forever. The protocol serializes writes, so one write stalled on a full
// receive window holds every subscription on the connection behind it, and
// the connection context carries no deadline of its own: without a write
// timeout nothing but a drain or an age limit, both off by default, ends it.
func TestWriteTimeoutReleasesAStalledPeer(t *testing.T) {
	var live atomic.Int64
	released := make(chan struct{})
	payload := strings.Repeat("x", 32<<10)
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { hello: String! }
		type Subscription { blast: String! }
	`),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Subscription(graphql.Subscribe("blast", func(ctx context.Context) (<-chan string, error) {
			live.Add(1)
			ch := make(chan string)
			go func() {
				defer close(ch)
				defer func() {
					if live.Add(-1) == 0 {
						close(released)
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
		t.Fatal(err)
	}

	c := dial(t, graphql.NewExecutor(s), gqlws.WithWriteTimeout(200*time.Millisecond))
	c.ws.SetReadLimit(1 << 20)
	c.init("")
	c.subscribe("1", `subscription { blast }`)
	if got := c.recv(); got.Type != "next" {
		t.Fatalf("frame = %+v, want next", got)
	}

	// Stop reading and let the socket back up.
	select {
	case <-released:
	case <-time.After(15 * time.Second):
		t.Fatalf("%d subscription(s) still running after the peer stopped reading", live.Load())
	}
}
