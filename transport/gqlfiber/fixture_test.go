package gqlfiber

import (
	"testing"

	"github.com/syssam/graphql-go"
)

func newTestExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Mutation { bump: String! }
`
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Mutation(graphql.Field("bump", func(graphql.Root) string { return "bumped" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}
