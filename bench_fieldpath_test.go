package graphql

import (
	"context"
	"testing"
)

// benchFieldPath runs the query BenchmarkExecuteUsers uses -- a list whose
// elements carry five pure leaf fields -- under whatever executor options are
// given, so the cost of observing a field can be read against the cost of not
// observing one.
func benchFieldPath(b *testing.B, opts ...ExecutorOption) {
	b.Helper()
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), f.options()...)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s, opts...)
	req := &Request{Query: `{ users { id name nick tags role } }`}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(ctx, req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

func BenchmarkFieldPathBare(b *testing.B) { benchFieldPath(b) }

// BenchmarkFieldPathInterceptor uses a no-op interceptor deliberately: it
// measures what the machinery costs before an observer does any work of its
// own.
func BenchmarkFieldPathInterceptor(b *testing.B) {
	benchFieldPath(b, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			return next(ctx)
		})))
}
