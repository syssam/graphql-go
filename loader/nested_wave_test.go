package loader_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

// The coordinator kept one stack of waves for the whole operation, and a park
// was counted in whichever wave was on top. Concurrent sibling subtrees push
// and pop in overlapping order, so here b parks into a's inner wave: x and y
// then finish without loading, that wave pops with b's park counted in it, and
// nothing ever flushes b's key. The request hung to its deadline.
//
// synctest makes the order exact: b loads only once x or y is running, and x
// and y return only after b is durably parked.
func TestLoadIsFlushedWhenASiblingSubtreeFinishesFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type obj struct{}
		var once sync.Once
		innerStarted := make(chan struct{})
		ld := loader.New(func(_ context.Context, keys []string) (map[string]string, error) {
			out := make(map[string]string, len(keys))
			for _, k := range keys {
				out[k] = "loaded " + k
			}
			return out, nil
		})
		// Once x or y runs, a's inner wave has been pushed and is on top.
		inner := func(context.Context, *obj) (string, error) {
			once.Do(func() { close(innerStarted) })
			time.Sleep(time.Second)
			return "done", nil
		}
		s, err := graphql.NewSchema(graphql.SDL(`
			type A { x: String! y: String! }
			type Query { a: A! b: String! }
		`),
			graphql.Object[obj]("A", graphql.Resolve("x", inner), graphql.Resolve("y", inner)),
			graphql.Query(
				graphql.Resolve("a", func(context.Context, graphql.Root) (*obj, error) { return &obj{}, nil }),
				graphql.Resolve("b", func(ctx context.Context, _ graphql.Root) (string, error) {
					<-innerStarted
					return ld.Load(ctx, "k")
				}),
			),
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		resp := graphql.NewExecutor(s).Execute(ctx, &graphql.Request{Query: `{ a { x y } b }`})
		if len(resp.Errors) > 0 {
			b, _ := json.Marshal(resp.Errors)
			t.Fatalf("errors: %s", b)
		}
		if got, want := string(resp.Data), `{"a":{"x":"done","y":"done"},"b":"loaded k"}`; got != want {
			t.Fatalf("data = %s, want %s", got, want)
		}
	})
}

// pop removed the top of the stack rather than the wave its caller pushed.
// Here a's inner wave is pushed before c's and finishes first, so a's pop
// removes c's wave. c's tasks then count against a's spent wave: q ends,
// taking ended past begun, and when p parks nothing can ever see the wave as
// ready or as spent, so p waited for its deadline.
func TestLoadIsFlushedWhenWavesPopOutOfOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type obj struct{}
		var aOnce, cOnce sync.Once
		aInner := make(chan struct{})
		cInner := make(chan struct{})
		ld := loader.New(func(_ context.Context, keys []string) (map[string]string, error) {
			out := make(map[string]string, len(keys))
			for _, k := range keys {
				out[k] = "loaded " + k
			}
			return out, nil
		})
		s, err := graphql.NewSchema(graphql.SDL(`
			type A { x: String! y: String! }
			type C { p: String! q: String! }
			type Query { a: A! c: C! }
		`),
			graphql.Object[obj]("A",
				graphql.Resolve("x", func(context.Context, *obj) (string, error) {
					aOnce.Do(func() { close(aInner) })
					<-cInner
					return "x", nil
				}),
				graphql.Resolve("y", func(context.Context, *obj) (string, error) {
					aOnce.Do(func() { close(aInner) })
					<-cInner
					return "y", nil
				}),
			),
			graphql.Object[obj]("C",
				graphql.Resolve("p", func(ctx context.Context, _ *obj) (string, error) {
					cOnce.Do(func() { close(cInner) })
					time.Sleep(2 * time.Second)
					return ld.Load(ctx, "p")
				}),
				graphql.Resolve("q", func(context.Context, *obj) (string, error) {
					cOnce.Do(func() { close(cInner) })
					time.Sleep(time.Second)
					return "q", nil
				}),
			),
			graphql.Query(
				graphql.Resolve("a", func(context.Context, graphql.Root) (*obj, error) { return &obj{}, nil }),
				// c's inner wave is pushed only once a's is on the stack.
				graphql.Resolve("c", func(context.Context, graphql.Root) (*obj, error) {
					<-aInner
					return &obj{}, nil
				}),
			),
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		resp := graphql.NewExecutor(s).Execute(ctx, &graphql.Request{Query: `{ a { x y } c { p q } }`})
		if len(resp.Errors) > 0 {
			b, _ := json.Marshal(resp.Errors)
			t.Fatalf("errors: %s", b)
		}
		if got, want := string(resp.Data), `{"a":{"x":"x","y":"y"},"c":{"p":"loaded p","q":"q"}}`; got != want {
			t.Fatalf("data = %s, want %s", got, want)
		}
	})
}
