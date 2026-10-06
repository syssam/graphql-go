package graphql

import (
	"context"
	"strings"
	"testing"
)

type mmFilter struct{ Name *string }

type mmOther struct{ Name *string }

type mmNode interface{ mmNode() }

type mmUser struct{}

func (*mmUser) mmNode() {}

type mmPost struct{}

func (*mmPost) mmNode() {}

// mmStray is bound to a member of Node and is not an mmNode.
type mmStray struct{}

// Each of these built cleanly and failed on the first request to reach it,
// which is what NewSchema exists to prevent: the mismatch is between two Go
// types the options themselves name.
func TestGoTypeMismatchesBetweenOptionsAreBuildErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		sdl  string
		opts []SchemaOption
		want []string
	}{
		{
			name: "an InputField setter for another struct",
			sdl:  `input Filter { name: String } type Query { f(in: Filter): Int }`,
			opts: []SchemaOption{
				Input[mmFilter]("Filter", InputField("name", func(o *mmOther, v *string) { o.Name = v })),
				Args[struct{ In *mmFilter }](),
				Query(FieldArgs("f", func(Root, struct{ In *mmFilter }) *int { return nil })),
			},
			want: []string{"Filter", "name", "mmOther", "mmFilter"},
		},
		{
			name: "an argument setter for another struct",
			sdl:  `type Query { f(name: String): Int }`,
			opts: []SchemaOption{
				Args[mmFilter](InputField("name", func(o *mmOther, v *string) { o.Name = v })),
				Query(FieldArgs("f", func(Root, mmFilter) *int { return nil })),
			},
			want: []string{"Query.f", `"name"`, "mmOther", "mmFilter"},
		},
		{
			name: "a TypeResolver for one member's type",
			sdl: `interface Node { id: ID } type User implements Node { id: ID } type Post implements Node { id: ID }
				type Query { node: Node }`,
			opts: []SchemaOption{
				Interface[mmNode]("Node", TypeResolver(func(*mmUser) string { return "User" })),
				Object[mmUser]("User", Field("id", func(*mmUser) *ID { return nil })),
				Object[mmPost]("Post", Field("id", func(*mmPost) *ID { return nil })),
				Query(Field("node", func(Root) mmNode { return &mmPost{} })),
			},
			want: []string{"Node", "TypeResolver", "mmUser", "mmNode"},
		},
		{
			name: "a field whose result the TypeResolver cannot take",
			sdl: `interface Node { id: ID } type User implements Node { id: ID } type Stray implements Node { id: ID }
				type Query { node: Node nodes: [Node] }`,
			opts: []SchemaOption{
				Interface[mmNode]("Node", TypeResolver(func(mmNode) string { return "User" })),
				Object[mmUser]("User", Field("id", func(*mmUser) *ID { return nil })),
				Object[mmStray]("Stray", Field("id", func(*mmStray) *ID { return nil })),
				Query(
					Field("node", func(Root) *mmStray { return &mmStray{} }),
					// *mmUser is an mmNode; the value mmUser is not.
					Field("nodes", func(Root) []mmUser { return nil }),
				),
			},
			want: []string{"Query.node", "Query.nodes", "TypeResolver", "*graphql.mmStray", "[]graphql.mmUser", "mmNode"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(SDL(tc.sdl), tc.opts...)
			if err == nil {
				t.Fatal("the schema built; the mismatch would surface on a request")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %s: %v", want, err)
				}
			}
		})
	}
}

// The shapes that are fine stay fine: a resolver over the interface type the
// binding names, and a shared Go type told apart by a TypeResolver.
func TestCompatibleAbstractBindingsStillBuild(t *testing.T) {
	_, err := NewSchema(SDL(`
		interface Node { id: ID } type User implements Node { id: ID } type Admin implements Node { id: ID }
		union Account = User | Admin
		type Query { node: Node account: Account }`),
		Interface[mmNode]("Node", TypeResolver(func(mmNode) string { return "User" })),
		Union[any]("Account", TypeResolver(func(any) string { return "Admin" })),
		Object[mmUser]("User", Field("id", func(*mmUser) *ID { return nil })),
		Object[mmUser]("Admin", Field("id", func(*mmUser) *ID { return nil })),
		Query(
			Field("node", func(Root) mmNode { return &mmUser{} }),
			Field("account", func(Root) any { return &mmUser{} }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
}

// What the build cannot see is a field error, not a lost response. A field
// typed any may hold a value the TypeResolver's parameter cannot, and the
// assertion that hands it over used to panic outside any resolver: the whole
// response became "internal system error", sibling fields included.
func TestAValueTheTypeResolverCannotTakeIsAFieldError(t *testing.T) {
	s, err := NewSchema(SDL(`
		interface Node { id: ID } type User implements Node { id: ID } type Stray implements Node { id: ID }
		type Query { ok: String! node: Node nodes: [Node] }`),
		Interface[mmNode]("Node", TypeResolver(func(mmNode) string { return "User" })),
		Object[mmUser]("User", Field("id", func(*mmUser) *ID { return nil })),
		Object[mmStray]("Stray", Field("id", func(*mmStray) *ID { return nil })),
		Query(
			Field("ok", func(Root) string { return "x" }),
			Resolve("node", func(context.Context, Root) (any, error) { return &mmStray{}, nil }),
			Resolve("nodes", func(context.Context, Root) ([]any, error) { return []any{&mmUser{}, &mmStray{}}, nil }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := run(t, NewExecutor(s), `{ ok node { id } nodes { id } }`, "")
	if got := string(resp.Data); got != `{"ok":"x","node":null,"nodes":[{"id":null},null]}` {
		t.Fatalf("data = %s (errors %s)", got, errorsJSON(resp.Errors))
	}
	if len(resp.Errors) != 2 {
		t.Fatalf("want one error per value, got %s", errorsJSON(resp.Errors))
	}
	for i, path := range []string{"node", "nodes[1]"} {
		e := resp.Errors[i]
		if e.Path.String() != path || !strings.Contains(e.Message, "TypeResolver") || !strings.Contains(e.Message, "mmStray") {
			t.Errorf("error %d = %s at %s, want one naming the TypeResolver and the value's type at %s", i, e.Message, e.Path.String(), path)
		}
	}
}
