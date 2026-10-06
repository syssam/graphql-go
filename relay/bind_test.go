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

func newConnSchema(t *testing.T) *graphql.Schema {
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
	return s
}

func newConnExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	return graphql.NewExecutor(newConnSchema(t))
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

// The cost model's connection support and this package describe the same
// shape from two sides; this is the test that they meet. Asking for ten
// times the page costs ten times as much, and only because the argument on
// the connection field is read.
func TestBoundConnectionIsPricedByItsPageSize(t *testing.T) {
	s := newConnSchema(t)
	cost := func(query string) int {
		t.Helper()
		e := graphql.NewExecutor(s, graphql.WithQueryCost(graphql.QueryCost{
			DefaultListSize: 10, Connections: true, Report: true,
		}))
		resp := e.Execute(context.Background(), &graphql.Request{Query: query})
		if len(resp.Errors) > 0 {
			t.Fatalf("errors: %v", resp.Errors)
		}
		return resp.Extensions["cost"].(map[string]any)["requestedQueryCost"].(int)
	}
	if got := cost(`{ users(first: 2) { edges { node { id name } } } }`); got != 9 {
		t.Fatalf("first: 2 cost %d, want 9", got)
	}
	if got := cost(`{ users(first: 20) { edges { node { id name } } } }`); got != 81 {
		t.Fatalf("first: 20 cost %d, want 81", got)
	}
}

// The Relay specification's own connection type declares edges nullable, and
// at a nullable position the engine writes a nil slice as null. A page with
// nothing on it is still a page: a client reading edges.length must not have
// to tell it from a failed field.
func TestAnEmptyPageIsAListWhereEdgesAreNullable(t *testing.T) {
	const nullableSDL = `
type PageInfo { hasNextPage: Boolean! hasPreviousPage: Boolean! startCursor: String endCursor: String }
type User { id: ID! name: String! }
type UserEdge { node: User cursor: String! }
type UserConnection { edges: [UserEdge] pageInfo: PageInfo! }
type Query { users(first: Int, after: String, last: Int, before: String): UserConnection }
`
	s, err := graphql.NewSchema(graphql.SDL(nullableSDL),
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
	for _, query := range []string{
		`{ users(first: 0) { edges { cursor } } }`,
		`{ users(after: "` + relay.OffsetCursor(2) + `") { edges { cursor } } }`,
	} {
		resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: query})
		if len(resp.Errors) > 0 {
			t.Fatalf("errors: %v", resp.Errors)
		}
		if got := string(resp.Data); got != `{"users":{"edges":[]}}` {
			t.Fatalf("%s\n got: %s\nwant an empty list", query, got)
		}
	}
}

// Most connections page one way. A field declaring only first and after has
// no last or before for Args to decode, and a binding that names an argument
// the schema lacks is a build error.
func TestForwardArgsBindAForwardOnlyConnection(t *testing.T) {
	const forwardSDL = `
type PageInfo { hasNextPage: Boolean! hasPreviousPage: Boolean! startCursor: String endCursor: String }
type User { id: ID! name: String! }
type UserEdge { node: User! cursor: String! }
type UserConnection { edges: [UserEdge!]! pageInfo: PageInfo! }
type Query { users(first: Int, after: String): UserConnection! }
`
	s, err := graphql.NewSchema(graphql.SDL(forwardSDL),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) graphql.ID { return graphql.ID(u.ID) }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		relay.Pagination(),
		relay.Bind[*user]("UserConnection", "UserEdge"),
		graphql.Query(graphql.ResolveArgs("users",
			func(_ context.Context, _ graphql.Root, a relay.ForwardArgs) (*relay.Connection[*user], error) {
				c, err := relay.FromSlice(people, a.Args())
				return &c, err
			})),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query: `{ users(first: 1, after: "` + relay.OffsetCursor(0) + `") { edges { node { id } } pageInfo { hasNextPage endCursor } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := `{"users":{"edges":[{"node":{"id":"2"}}],"pageInfo":{"hasNextPage":true,"endCursor":"` + relay.OffsetCursor(1) + `"}}}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}

	// Absent, both arguments are nil rather than zero, as in Args: first: 0
	// asks for no edges and no first at all asks for every one.
	resp = e.Execute(context.Background(), &graphql.Request{Query: `{ users { edges { node { id } } } }`})
	if got := string(resp.Data); got != `{"users":{"edges":[{"node":{"id":"1"}},{"node":{"id":"2"}},{"node":{"id":"3"}}]}}` {
		t.Fatalf("no arguments: %s", got)
	}
}
