package relay_test

import (
	"context"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/relay"
)

type user struct {
	ID   string
	Name string
}

const nodeSDL = `
interface Node { id: ID! }
type User implements Node { id: ID! name: String! }
type Query { node(id: ID!): Node }
`

func newNodeExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	users := map[string]*user{"123": {ID: "123", Name: "Ada"}}
	s, err := graphql.NewSchema(graphql.SDL(nodeSDL),
		graphql.Object[user]("User",
			relay.IDField("User", func(u *user) string { return u.ID }),
			graphql.Field("name", func(u *user) string { return u.Name }),
		),
		relay.Node(func(_ context.Context, typeName, id string) (any, error) {
			if typeName == "User" {
				if u, ok := users[id]; ok {
					return u, nil
				}
			}
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

// The id a Node reports must be the one Query.node accepts back, or the
// round trip a Relay client depends on does not close.
func TestNodeRoundTripsItsOwnID(t *testing.T) {
	e := newNodeExecutor(t)
	gid := relay.ToGlobalID("User", "123")

	resp := e.Execute(context.Background(), &graphql.Request{
		Query:     `query Q($id: ID!) { node(id: $id) { id ... on User { name } } }`,
		Variables: []byte(`{"id":"` + string(gid) + `"}`),
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := `{"node":{"id":"` + string(gid) + `","name":"Ada"}}`
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

// An id this server never issued is a client error, not a panic and not a
// lookup against a garbage type name.
func TestNodeRejectsAMalformedID(t *testing.T) {
	e := newNodeExecutor(t)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query:     `query Q($id: ID!) { node(id: $id) { id } }`,
		Variables: []byte(`{"id":"not-base64!!"}`),
	})
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %d, want 1: %v", len(resp.Errors), resp.Errors)
	}
	if !strings.Contains(resp.Errors[0].Message, "global id") {
		t.Fatalf("error message = %q", resp.Errors[0].Message)
	}
}

// An unknown but well-formed id is simply not found: node is nullable.
func TestNodeReturnsNullForAnUnknownID(t *testing.T) {
	e := newNodeExecutor(t)
	resp := e.Execute(context.Background(), &graphql.Request{
		Query:     `query Q($id: ID!) { node(id: $id) { id } }`,
		Variables: []byte(`{"id":"` + string(relay.ToGlobalID("User", "999")) + `"}`),
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"node":null}` {
		t.Fatalf("data = %s", got)
	}
}
