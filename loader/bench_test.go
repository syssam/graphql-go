package loader_test

import (
	"context"
	"strconv"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

type benchItem struct{ Owner int }

// A list of 100 rows whose owner field loads through one Loader: the shape a
// DataLoader exists for, and the path the batch context is built on.
func benchLoaderExecutor(b *testing.B, opts ...graphql.ExecutorOption) *graphql.Executor {
	b.Helper()
	ld := loader.New(func(_ context.Context, keys []int) (map[int]string, error) {
		out := make(map[int]string, len(keys))
		for _, k := range keys {
			out[k] = strconv.Itoa(k)
		}
		return out, nil
	})
	items := make([]*benchItem, 100)
	for i := range items {
		items[i] = &benchItem{Owner: i % 10}
	}
	s, err := graphql.NewSchema(graphql.SDL(`type Item { owner: String! } type Query { items: [Item!]! }`),
		graphql.Object[benchItem]("Item",
			graphql.Resolve("owner", func(ctx context.Context, it *benchItem) (string, error) {
				return ld.Load(ctx, it.Owner)
			}),
		),
		graphql.Query(graphql.Field("items", func(graphql.Root) []*benchItem { return items })),
	)
	if err != nil {
		b.Fatal(err)
	}
	return graphql.NewExecutor(s, opts...)
}

func benchLoader(b *testing.B, e *graphql.Executor) {
	req := &graphql.Request{Query: `{ items { owner } }`}
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

func BenchmarkLoaderSharedContext(b *testing.B) { benchLoader(b, benchLoaderExecutor(b)) }

// A field interceptor gives every resolver a context of its own, so a batch
// has one distinct waiter context per row: the most a batch context costs.
func BenchmarkLoaderDistinctContexts(b *testing.B) {
	type key struct{}
	benchLoader(b, benchLoaderExecutor(b, graphql.WithFieldInterceptor(graphql.FieldInterceptorFunc(
		func(ctx context.Context, fc *graphql.FieldContext, next graphql.FieldHandler) (any, error) {
			return next(context.WithValue(ctx, key{}, fc))
		}))))
}
