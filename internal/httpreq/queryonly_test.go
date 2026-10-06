package httpreq

import (
	"context"
	"testing"

	"github.com/syssam/graphql-go"
)

// The refusal is recognised by the code the executor puts on it, so the
// executor is what this runs: a response built here by hand would agree with
// QueryOnlyRefused about a code the executor had stopped sending.
func TestQueryOnlyRefusedRecognisesTheExecutorsRefusal(t *testing.T) {
	ran := 0
	s, err := graphql.NewSchema(graphql.SDL("type Query { hello: String! }\ntype Mutation { bump: String! }"),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Mutation(graphql.Field("bump", func(graphql.Root) string {
			ran++
			return "bumped"
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	// A presenter that rewrites every error: the refusal has to be found
	// whatever an application does to the errors it shows.
	e := graphql.NewExecutor(s, graphql.WithErrorPresenter(func(context.Context, error) *graphql.Error {
		return graphql.Errorf("masked")
	}))
	execute := func(query string, queryOnly bool) *graphql.Response {
		return e.Execute(context.Background(), &graphql.Request{Query: query, QueryOnly: queryOnly})
	}

	if resp := execute(`mutation { bump }`, true); !QueryOnlyRefused(resp) {
		t.Errorf("the executor's refusal was not recognised: %+v", resp.Errors)
	}
	if ran != 0 {
		t.Errorf("the refused mutation ran %d time(s)", ran)
	}
	for name, resp := range map[string]*graphql.Response{
		"a query":             execute(`{ hello }`, true),
		"a validation error":  execute(`{ nope }`, true),
		"an unmarked request": execute(`mutation { bump }`, false),
		"no response":         nil,
	} {
		if QueryOnlyRefused(resp) {
			t.Errorf("%s was taken for a refusal", name)
		}
	}
}
