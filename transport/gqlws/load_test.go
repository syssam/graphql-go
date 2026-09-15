package gqlws_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// A subscription server's failure mode at scale is not slowness, it is
// accumulation: a goroutine or a channel kept per connection that outlives it.
// A thousand connections opened and closed over a day is the shape that finds
// it, and nothing else in this suite opens more than one connection.

// fanoutSDL drives every subscriber from one broadcaster, so a test can hold
// many connections open and feed them all at once.
const fanoutSDL = `
type Tick { n: Int! }
type Query { ping: String! }
type Subscription { ticks: Tick! }
`

type tick struct{ N int }

// fanout hands every subscriber its own channel and drops slow ones, the same
// contract the example's broker documents.
type fanout struct {
	mu   sync.Mutex
	next int
	subs map[int]chan *tick
}

func (f *fanout) subscribe(ctx context.Context) <-chan *tick {
	ch := make(chan *tick, 4)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = map[int]chan *tick{}
	}
	id := f.next
	f.next++
	f.subs[id] = ch
	f.mu.Unlock()

	go func() {
		<-ctx.Done()
		f.mu.Lock()
		if c, ok := f.subs[id]; ok {
			delete(f.subs, id)
			close(c)
		}
		f.mu.Unlock()
	}()
	return ch
}

func (f *fanout) publish(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		select {
		case ch <- &tick{N: n}:
		default:
		}
	}
}

func (f *fanout) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func fanoutServer(t testing.TB) (*fanout, *httptest.Server) {
	t.Helper()
	f := &fanout{}
	s, err := graphql.NewSchema(graphql.SDL(fanoutSDL),
		graphql.Object[tick]("Tick", graphql.Field("n", func(v *tick) int { return v.N })),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Subscription(graphql.Subscribe("ticks", func(ctx context.Context) (<-chan *tick, error) {
			return f.subscribe(ctx), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gqlws.New(graphql.NewExecutor(s), gqlws.WithPingInterval(0)))
	t.Cleanup(srv.Close)
	return f, srv
}

// openSubscriber dials, handshakes and subscribes, returning the connection.
func openSubscriber(t testing.TB, url, id string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{gqlws.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	send := func(v any) {
		b, _ := json.Marshal(v)
		if werr := ws.Write(ctx, websocket.MessageText, b); werr != nil {
			t.Fatalf("write: %v", werr)
		}
	}
	send(frame{Type: "connection_init"})
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("ack: %v", err)
	}
	send(frame{ID: id, Type: "subscribe",
		Payload: json.RawMessage(`{"query":"subscription { ticks { n } }"}`)})
	return ws
}

// settledGoroutines waits for the count to stop falling, then reports it.
// Connection teardown is asynchronous on both sides, so a single reading taken
// straight after Close is measuring the shutdown rather than what is left.
func settledGoroutines(target int, within time.Duration) int {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		runtime.GC()
		if n := runtime.NumGoroutine(); n <= target {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	runtime.GC()
	return runtime.NumGoroutine()
}

// TestManySubscriptionsAreReleased is the accumulation test: open a batch of
// subscriptions, drive events through all of them, close them, and require
// both the source registrations and the goroutines to come back.
func TestManySubscriptionsAreReleased(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test")
	}
	const n = 150

	f, srv := fanoutServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	// One connection first, so the baseline includes whatever the server and
	// client allocate once rather than counting it as a leak.
	warm := openSubscriber(t, url, "warm")
	f.publish(0)
	time.Sleep(100 * time.Millisecond)
	warm.Close(websocket.StatusNormalClosure, "")
	settledGoroutines(0, 2*time.Second)
	baseline := runtime.NumGoroutine()

	conns := make([]*websocket.Conn, 0, n)
	for i := range n {
		conns = append(conns, openSubscriber(t, url, fmt.Sprintf("s%d", i)))
	}

	// Wait for the server to have registered them all, rather than assuming.
	deadline := time.Now().Add(20 * time.Second)
	for f.open() < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.open(); got != n {
		t.Fatalf("%d subscriptions registered, want %d", got, n)
	}

	for i := range 5 {
		f.publish(i)
	}

	for _, c := range conns {
		c.Close(websocket.StatusNormalClosure, "")
	}

	// Every source registration must be dropped, which is the executor
	// cancelling each operation as its connection ends.
	for f.open() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.open(); got != 0 {
		t.Fatalf("%d subscriptions still registered after every client closed", got)
	}

	// Goroutines are the other half: a source can deregister while the
	// goroutine that was feeding it stays parked forever.
	got := settledGoroutines(baseline+10, 20*time.Second)
	if got > baseline+10 {
		t.Fatalf("goroutines %d, baseline %d: %d subscriptions leaked about %.1f goroutines each",
			got, baseline, n, float64(got-baseline)/float64(n))
	}
	t.Logf("%d subscriptions: goroutines %d -> %d (baseline %d)", n, baseline+n, got, baseline)
}

// BenchmarkSubscriptionFanout measures one broadcast reaching every
// subscriber: publish, then wait until all of them have the event.
//
// Waiting is the point. publish drops into a full buffer rather than blocking,
// so timing it alone measures a channel send and reports a fan-out to 128
// clients as costing 45ns each -- most of those events never reached anyone.
// Counting receipts measures the executor, the encoder and the socket write,
// which is what a broadcast actually costs.
func BenchmarkSubscriptionFanout(b *testing.B) {
	for _, subscribers := range []int{1, 16, 128} {
		b.Run(fmt.Sprintf("subscribers=%d", subscribers), func(b *testing.B) {
			f, srv := fanoutServer(b)
			url := "ws" + strings.TrimPrefix(srv.URL, "http")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			conns := make([]*websocket.Conn, 0, subscribers)
			for i := range subscribers {
				conns = append(conns, openSubscriber(b, url, fmt.Sprintf("s%d", i)))
			}
			defer func() {
				for _, c := range conns {
					c.Close(websocket.StatusNormalClosure, "")
				}
			}()
			for f.open() < subscribers {
				time.Sleep(10 * time.Millisecond)
			}

			received := make(chan struct{}, subscribers*4)
			var wg sync.WaitGroup
			for _, c := range conns {
				wg.Add(1)
				go func(c *websocket.Conn) {
					defer wg.Done()
					for ctx.Err() == nil {
						if _, _, err := c.Read(ctx); err != nil {
							return
						}
						select {
						case received <- struct{}{}:
						case <-ctx.Done():
							return
						}
					}
				}(c)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				f.publish(i)
				for range subscribers {
					select {
					case <-received:
					case <-time.After(30 * time.Second):
						b.Fatal("timed out waiting for a broadcast to land")
					}
				}
			}
			b.StopTimer()
			cancel()
			wg.Wait()
		})
	}
}

// TestIdleSubscriptionsAreReleased is the harder half, and the one a naive
// leak test misses. Writes use the connection context, so a subscription with
// an event pending is reclaimed by that write failing the moment the client
// goes — which passes a leak test whether or not cancellation actually
// propagates. A subscription that is idle when its client disconnects has no
// such backstop: only the operation context reaching the source releases it.
func TestIdleSubscriptionsAreReleased(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test")
	}
	const n = 150

	f, srv := fanoutServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	warm := openSubscriber(t, url, "warm")
	deadline := time.Now().Add(20 * time.Second)
	for f.open() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	warm.Close(websocket.StatusNormalClosure, "")
	settledGoroutines(0, 2*time.Second)
	baseline := runtime.NumGoroutine()

	conns := make([]*websocket.Conn, 0, n)
	for i := range n {
		conns = append(conns, openSubscriber(t, url, fmt.Sprintf("s%d", i)))
	}
	deadline = time.Now().Add(20 * time.Second)
	for f.open() < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.open(); got != n {
		t.Fatalf("%d subscriptions registered, want %d", got, n)
	}

	// Nothing is published: every subscription is parked on its source when
	// its client disappears.
	for _, c := range conns {
		c.Close(websocket.StatusNormalClosure, "")
	}

	for f.open() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.open(); got != 0 {
		t.Fatalf("%d idle subscriptions still registered after every client closed", got)
	}
	if got := settledGoroutines(baseline+10, 20*time.Second); got > baseline+10 {
		t.Fatalf("goroutines %d, baseline %d: %d idle subscriptions leaked about %.1f goroutines each",
			got, baseline, n, float64(got-baseline)/float64(n))
	}
}
