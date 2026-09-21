package loader_test

import (
	"context"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

// An Inline() resolver is not schedulable, so writeFieldsConcurrent writes it
// on the caller's goroutine after g.wait() -- with its wave still pushed and
// every announced task already ended. A Load from there parks into a wave that
// can never become ready again (ready() requires an in-flight task), and Park
// does not arm its scheduler fallback because the stack is not empty. The
// request then hangs until its deadline, holding a concurrency slot the whole
// executor shares.
func TestLoadFromAnInlineResolverIsFlushed(t *testing.T) {
	ld := loader.New(func(_ context.Context, ks []string) (map[string]string, error) {
		out := make(map[string]string, len(ks))
		for _, k := range ks {
			out[k] = "v-" + k
		}
		return out, nil
	})
	s, err := graphql.NewSchema(graphql.SDL(`
		type Query { thing: Thing! }
		type Thing { a: String! b: String! viaLoader: String! }
	`),
		graphql.Object[thing]("Thing",
			graphql.Resolve("a", func(context.Context, *thing) (string, error) { return "a", nil }),
			graphql.Resolve("b", func(context.Context, *thing) (string, error) { return "b", nil }),
			// Inline, so it runs after the wave's tasks have all ended.
			graphql.Resolve("viaLoader", func(ctx context.Context, _ *thing) (string, error) {
				return ld.Load(ctx, "k1")
			}, graphql.Inline()),
		),
		graphql.Query(graphql.Field("thing", func(graphql.Root) *thing { return &thing{} })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan *graphql.Response, 1)
	go func() { done <- e.Execute(ctx, &graphql.Request{Query: `{ thing { a b viaLoader } }`}) }()
	select {
	case resp := <-done:
		if len(resp.Errors) != 0 {
			t.Fatalf("errors = %v", resp.Errors)
		}
		const want = `{"thing":{"a":"a","b":"b","viaLoader":"v-k1"}}`
		if got := string(resp.Data); got != want {
			t.Fatalf("data = %s, want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request hung: a Load from an inline resolver parked into a wave that can never be ready")
	}
}

type thing struct{}
