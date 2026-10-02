package graphql

import (
	"context"
	"testing"
)

type benchAggFilter struct {
	Name  *string
	Limit *int
}

type benchAggArgs struct {
	IDs    []int
	Filter *benchAggFilter
}

// A list or input-object literal is decoded on every request, because decoded
// once it is one slice or struct shared by every request. This is what that
// costs, against BenchmarkExecuteConstantArgs, whose scalar literals are still
// decoded once.
func BenchmarkExecuteLiteralAggregateArgs(b *testing.B) {
	s, err := NewSchema(SDL(`
		input Filter { name: String limit: Int }
		type Query { rows(ids: [Int!], filter: Filter): String! }
	`),
		Input[benchAggFilter]("Filter"),
		Args[benchAggArgs](),
		Query(ResolveArgs("rows", func(context.Context, Root, benchAggArgs) (string, error) { return "ok", nil })),
	)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s)
	req := &Request{Query: `{ rows(ids: [1, 2, 3], filter: {name: "ada", limit: 10}) }`}
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(context.Background(), req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}
