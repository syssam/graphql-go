package graphql

import (
	"context"
	"testing"
)

// BenchmarkExecuteTypenameHeavy measures __typename-dense responses, the shape
// Apollo clients produce, with no authorizer registered.
func BenchmarkExecuteTypenameHeavy(b *testing.B) {
	_, e := newFixtureExecutor(b)
	ctx := context.Background()
	req := &Request{Query: `{ users { __typename id name friends { __typename id } } }`}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Execute(ctx, req).Release()
	}
}
