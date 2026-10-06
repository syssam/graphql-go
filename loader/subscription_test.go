package loader_test

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

// The function bound with Subscribe is told to feed its channel from a
// goroutine, and the context it is given is the one that goroutine has. That
// context carried the subscription's operation, which a Loader took for a
// request and cached against -- for the life of the stream, so every event
// after the first was built from what the first one loaded.
func TestALoaderInASubscriptionsSourceIsNotCachedForTheStream(t *testing.T) {
	var version, batches atomic.Int64
	ld := loader.New(func(_ context.Context, keys []string) (map[string]string, error) {
		batches.Add(1)
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			out[k] = "v" + strconv.FormatInt(version.Load(), 10)
		}
		return out, nil
	})
	tick := make(chan struct{})
	defer close(tick)
	s, err := graphql.NewSchema(graphql.SDL(`type Query { ping: String! } type Subscription { price: String! }`),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Subscription(graphql.Subscribe("price", func(ctx context.Context) (<-chan string, error) {
			ch := make(chan string)
			go func() {
				defer close(ch)
				for range tick {
					v, err := ld.Load(ctx, "AAPL")
					if err != nil {
						return
					}
					select {
					case ch <- v:
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := graphql.NewExecutor(s).Subscribe(ctx, &graphql.Request{Query: `subscription { price }`})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		version.Store(int64(i))
		tick <- struct{}{}
		select {
		case resp := <-out:
			if got, want := string(resp.Data), `{"price":"v`+strconv.Itoa(i)+`"}`; got != want {
				t.Fatalf("event %d: data = %s, want %s: the source was answered from an earlier event's load", i, got, want)
			}
			resp.Release()
		case <-time.After(5 * time.Second):
			t.Fatalf("no event %d", i)
		}
	}
	if n := batches.Load(); n != 3 {
		t.Fatalf("batches = %d, want one per event", n)
	}
}
