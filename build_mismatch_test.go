package graphql

import (
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
