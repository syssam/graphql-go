package relay_test

import (
	"context"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/relay"
)

const connSDL = `
type PageInfo { hasNextPage: Boolean! hasPreviousPage: Boolean! startCursor: String endCursor: String }
type User { id: ID! name: String! }
type UserEdge { node: User! cursor: String! }
type UserConnection { edges: [UserEdge!]! pageInfo: PageInfo! }
type Query { users(first: Int, after: String, last: Int, before: String): UserConnection! }
`

var people = []*user{{ID: "1", Name: "Ada"}, {ID: "2", Name: "Alan"}, {ID: "3", Name: "Grace"}}

func newConnExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(connSDL),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		relay.Pagination(),
		relay.Bind[*user]("UserConnection", "UserEdge"),
		graphql.Query(graphql.ResolveArgs("users",
			func(_ context.Context, _ graphql.Root, a relay.Args) (*relay.Connection[*user], error) {
				c, err := relay.FromSlice(people, a)
				return &c, err
			})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

func TestBoundConnectionServesAPage(t *testing.T) {
	e := newConnExecutor(t)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query: `{ users(first: 2) { edges { cursor node { id name } } pageInfo { hasNextPage hasPreviousPage endCursor } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := `{"users":{"edges":[` +
		`{"cursor":"` + relay.OffsetCursor(0) + `","node":{"id":"1","name":"Ada"}},` +
		`{"cursor":"` + relay.OffsetCursor(1) + `","node":{"id":"2","name":"Alan"}}],` +
		`"pageInfo":{"hasNextPage":true,"hasPreviousPage":false,"endCursor":"` + relay.OffsetCursor(1) + `"}}}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

// An empty page must render startCursor and endCursor as null, not "".
func TestBoundConnectionRendersNullCursorsWhenEmpty(t *testing.T) {
	e := newConnExecutor(t)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query: `{ users(first: 0) { edges { cursor } pageInfo { startCursor endCursor } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := `{"users":{"edges":[],"pageInfo":{"startCursor":null,"endCursor":null}}}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}
