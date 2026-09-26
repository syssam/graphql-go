package fed_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

// A code generator embeds a schema as several files. SubgraphFS serves them
// as one subgraph: entities declared in one file resolve, and _service
// returns every file, in path order, which is the text the router composes.
func TestSubgraphFSServesSeveralFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"schema/b_query.graphql": {Data: []byte("type Query { me: User }\n")},
		"schema/a_user.graphql":  {Data: []byte("type User @key(fields: \"id\") { id: ID! name: String! }\n")},
		"schema/notes.txt":       {Data: []byte("not SDL")},
	}
	// Two patterns in reverse order: the files still come in path order.
	src, bindings, err := fed.SubgraphFS(fsys, []string{"schema/b_*.graphql", "schema/a_*.graphql"},
		fed.Resolver("User", func(_ context.Context, r fed.Representation) (*user, error) {
			id, _ := r.ID("id")
			return &user{ID: string(id), Name: "u" + string(id)}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := graphql.NewSchema(src, bindings,
		graphql.Object[graphql.Root]("Query", graphql.Resolve("me", func(context.Context, graphql.Root) (*user, error) { return nil, nil })),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		))
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	r := e.Execute(context.Background(), &graphql.Request{Query: `{ _service { sdl } _entities(representations: [{__typename: "User", id: "7"}]) { ... on User { name } } }`})
	if len(r.Errors) > 0 {
		t.Fatal(r.Errors)
	}
	got := string(r.Data)
	if !strings.Contains(got, `"name":"u7"`) {
		t.Errorf("entity not resolved: %s", got)
	}
	a, b := strings.Index(got, "type User"), strings.Index(got, "type Query")
	if a < 0 || b < 0 || a > b || strings.Contains(got, "not SDL") {
		t.Errorf("_service must return the .graphql files in path order and nothing else: %s", got)
	}

	if _, _, err := fed.SubgraphFS(fsys, []string{"nothing/*.graphql"}); err == nil {
		t.Error("a pattern matching no file must be an error, not an empty subgraph")
	}
}
