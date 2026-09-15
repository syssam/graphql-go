package graphql

import (
	"context"
	"iter"
	"strings"
	"testing"
)

type tUser struct {
	ID   string
	Name string
	Nick *string
	Tags []string
}

const objectSDL = `
type User { id: ID! name: String! nick: String tags: [String!]! friends: [User!]! }
type Query { me: User! users: [User] }
`

func userFields() []FieldOption {
	return []FieldOption{
		Field("id", func(u *tUser) string { return u.ID }),
		Field("name", func(u *tUser) string { return u.Name }),
		Field("nick", func(u *tUser) *string { return u.Nick }),
		Field("tags", func(u *tUser) []string { return u.Tags }),
		Resolve("friends", func(_ context.Context, u *tUser) ([]*tUser, error) { return nil, nil }),
	}
}

func TestObjectBindingsCompose(t *testing.T) {
	s, err := NewSchema(SDL(objectSDL),
		Object[tUser]("User", userFields()...),
		Object[Root]("Query",
			Resolve("me", func(context.Context, Root) (*tUser, error) { return &tUser{ID: "1"}, nil }),
			Field("users", func(Root) []*tUser { return nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	user := s.objects["User"]
	if user == nil || len(user.fields) != 5 {
		t.Fatalf("user fields = %v", user)
	}
	if !user.fields["id"].leaf || !user.fields["id"].pure || user.fields["id"].schedulable {
		t.Fatal("id should be a pure inline leaf")
	}
	if user.fields["friends"].leaf || user.fields["friends"].pure || !user.fields["friends"].schedulable {
		t.Fatal("friends should be a schedulable composite resolver")
	}
	if !user.hasSchedulable || s.query.hasSchedulable != true {
		t.Fatal("hasSchedulable flags not set")
	}
	if len(s.goTypes[user.shapes.ptr]) != 1 || s.goTypes[user.shapes.ptr][0] != user ||
		len(s.goTypes[user.shapes.elem]) != 1 || s.goTypes[user.shapes.elem][0] != user {
		t.Fatal("goTypes must map both E and *E")
	}
}

func TestObjectMergeAndValueParent(t *testing.T) {
	s, err := NewSchema(SDL(objectSDL),
		Object[tUser]("User", userFields()[:2]...),
		Object[tUser]("User", userFields()[2:]...),
		Object[Root]("Query",
			Resolve("me", func(context.Context, Root) (tUser, error) { return tUser{ID: "1"}, nil }),
			Field("users", func(*Root) []tUser { return nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.objects["User"].fields) != 5 {
		t.Fatal("merged bindings must cover all fields")
	}
	me := s.query.fields["me"]
	if me.shape == nil || me.shape.toPtr == nil {
		t.Fatal("value-typed result must carry a toPtr adapter")
	}
}

func TestObjectValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		opts []SchemaOption
		want string
	}{
		{
			"pointer type parameter",
			[]SchemaOption{Object[*tUser]("User")},
			"not the pointer type",
		},
		{
			"leaf type mismatch",
			[]SchemaOption{Object[tUser]("User", Field("id", func(u *tUser) bool { return false }))},
			"User.id",
		},
		{
			"list depth mismatch",
			[]SchemaOption{Object[tUser]("User", Field("tags", func(u *tUser) string { return "" }))},
			"list level",
		},
		{
			"unknown field",
			[]SchemaOption{Object[tUser]("User", Field("nope", func(u *tUser) string { return "" }))},
			"User.nope is not defined",
		},
		{
			"duplicate field",
			[]SchemaOption{Object[tUser]("User", Field("id", func(u *tUser) string { return "" }), Field("id", func(u *tUser) string { return "" }))},
			"bound more than once",
		},
		{
			"wrong parent type",
			[]SchemaOption{Object[tUser]("User", Field("id", func(u *Root) string { return "" }))},
			"parent type",
		},
		{
			"composite type mismatch",
			[]SchemaOption{Object[tUser]("User", Resolve("friends", func(context.Context, *tUser) ([]*Root, error) { return nil, nil }))},
			"does not match Object[User]",
		},
		{
			"root bound to non-Root",
			[]SchemaOption{Object[tUser]("Query")},
			"graphql.Root",
		},
		{
			"unknown object",
			[]SchemaOption{Object[tUser]("Nope")},
			`Object "Nope"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(SDL(objectSDL), tc.opts...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestObjectSameGoTypeTwice(t *testing.T) {
	err := Validate(SDL(`type A { x: Int! } type B { x: Int! } type Query { a: A b: B }`),
		Object[tUser]("A", Field("x", func(*tUser) int { return 0 })),
		Object[tUser]("B", Field("x", func(*tUser) int { return 0 })),
		Object[Root]("Query", Field("a", func(Root) *tUser { return nil }), Field("b", func(Root) *tUser { return nil })),
	)
	if err != nil {
		t.Fatalf("one Go type may back two objects, got %v", err)
	}
}

func TestFieldOpts(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int! b: Int! }`),
		Object[Root]("Query",
			Field("a", func(Root) int { return 1 }, Concurrent()),
			Resolve("b", func(context.Context, Root) (int, error) { return 1, nil }, Inline()),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !s.query.fields["a"].schedulable {
		t.Fatal("Concurrent() must make a pure field schedulable")
	}
	if s.query.fields["b"].schedulable {
		t.Fatal("Inline() must keep a resolver inline")
	}
}

func TestSeqShapeAcceptedAtBuild(t *testing.T) {
	type post struct{ Title string }
	s, err := NewSchema(SDL(`type Post { title: String! } type Query { posts: [Post!]! }`),
		Object[post]("Post",
			Field("title", func(v *post) string { return v.Title }),
		),
		Query(
			Resolve("posts", func(ctx context.Context, _ Root) (iter.Seq[*post], error) {
				return nil, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema rejected an iter.Seq result: %v", err)
	}
	if s == nil {
		t.Fatal("nil schema")
	}
}

// The seq and slice spellings of the same list must be indistinguishable in
// the response, which is the whole contract of this feature.
func TestSeqListMatchesSliceList(t *testing.T) {
	_, e := newFixtureExecutor(t)
	slice := run(t, e, `{users{id name}}`, "")
	seq := run(t, e, `{usersSeq{id name}}`, "")
	if len(seq.Errors) != 0 {
		t.Fatalf("seq list errored: %v", seq.Errors)
	}
	want := strings.Replace(string(slice.Data), `"users"`, `"usersSeq"`, 1)
	if got := string(seq.Data); got != want {
		t.Fatalf("seq list = %s, want %s", got, want)
	}
}

// The spec puts Field/FieldArgs in scope alongside Resolve. Shapes are
// registered per Go type, not per constructor, so a pure field must accept a
// seq too; this pins that rather than assuming it.
func TestSeqListFromPureField(t *testing.T) {
	type post struct{ Title string }
	posts := []*post{{Title: "a"}, {Title: "b"}}
	s, err := NewSchema(SDL(`type Post { title: String! } type Query { posts: [Post!]! }`),
		Object[post]("Post", Field("title", func(v *post) string { return v.Title })),
		Query(Field("posts", func(_ Root) iter.Seq[*post] {
			return func(yield func(*post) bool) {
				for _, p := range posts {
					if !yield(p) {
						return
					}
				}
			}
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema rejected a pure seq field: %v", err)
	}
	resp := run(t, NewExecutor(s), `{posts{title}}`, "")
	if got := string(resp.Data); got != `{"posts":[{"title":"a"},{"title":"b"}]}` {
		t.Fatalf("data = %s", got)
	}
}
