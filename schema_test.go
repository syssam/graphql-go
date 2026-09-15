package graphql

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestNewSchemaMinimal(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int }`),
		Object[Root]("Query", Field("a", func(Root) int { return 1 })),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.AST().Query == nil || s.AST().Query.Name != "Query" {
		t.Fatal("query type not resolved")
	}
	if s.query == nil || s.query.fields["a"] == nil {
		t.Fatal("query object not built")
	}
}

func TestNewSchemaInvalidSDL(t *testing.T) {
	_, err := NewSchema(SDL(`type Query { a: Missing }`))
	if err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Fatalf("expected undefined type error, got %v", err)
	}
}

func TestNewSchemaNoSources(t *testing.T) {
	if _, err := NewSchema(Source{}); err == nil {
		t.Fatal("expected error for empty source")
	}
}

func TestSDLFS(t *testing.T) {
	fsys := fstest.MapFS{
		"schema/query.graphql": {Data: []byte(`type Query { a: A }`)},
		"schema/a.graphql":     {Data: []byte(`type A { id: ID! }`)},
		"schema/notes.txt":     {Data: []byte(`ignored`)},
	}
	type A struct{ ID string }
	s, err := NewSchema(SDLFS(fsys, "schema/*.graphql"),
		Object[Root]("Query", Field("a", func(Root) *A { return nil })),
		Object[A]("A", Field("id", func(a *A) string { return a.ID })),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.AST().Types["A"] == nil {
		t.Fatal("type A from second file missing")
	}
}

func TestSourcesCombinesFileSystems(t *testing.T) {
	queryFS := fstest.MapFS{
		"query.graphql": {Data: []byte(`type Query { a: A }`)},
	}
	typeFS := fstest.MapFS{
		"a.graphql": {Data: []byte(`type A { id: ID! }`)},
	}
	type A struct{ ID string }
	s, err := NewSchema(Sources(SDLFS(queryFS, "*.graphql"), SDLFS(typeFS, "*.graphql")),
		Object[Root]("Query", Field("a", func(Root) *A { return nil })),
		Object[A]("A", Field("id", func(a *A) string { return a.ID })),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.AST().Types["A"] == nil {
		t.Fatal("type A from second file system missing")
	}
}

func TestOptionsJoinsBindings(t *testing.T) {
	type A struct{ ID string }
	s, err := NewSchema(SDL(`type A { id: ID! } type Query { a: A }`),
		Options(
			Object[A]("A", Field("id", func(a *A) string { return a.ID })),
			Query(Field("a", func(Root) *A { return &A{ID: "1"} })),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.objects["A"] == nil || s.query == nil {
		t.Fatal("joined options did not bind types")
	}
}

func TestPrintSDLRoundTrip(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int }`),
		Object[Root]("Query", Field("a", func(Root) int { return 1 })),
	)
	if err != nil {
		t.Fatal(err)
	}
	printed := PrintSDL(s)
	if !strings.Contains(printed, "type Query") {
		t.Fatalf("PrintSDL output missing type: %s", printed)
	}
	if _, err := NewSchema(SDL(printed), Object[Root]("Query", Field("a", func(Root) int { return 1 }))); err != nil {
		t.Fatalf("printed SDL does not reload: %v", err)
	}
}

func TestDisableIntrospection(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int }`),
		Object[Root]("Query", Field("a", func(Root) int { return 1 })),
		DisableIntrospection(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.IntrospectionEnabled() {
		t.Fatal("introspection should be disabled")
	}
}

func TestNewSchemaAggregatesErrors(t *testing.T) {
	_, err := NewSchema(SDL(`type Query { a: Int b: String } type Unbound { x: Int }`),
		Object[Root]("Query", Field("a", func(Root) string { return "" })),
	)
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"Query.a", "Query.b", "Unbound"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q:\n%s", want, msg)
		}
	}
}

type depArgs struct{ A int32 }
type depNullArgs struct{ A *int32 }
type depIn struct{ A int32 }
type depInArgs struct{ I depIn }

// TestNewSchemaRejectsDeprecatedRequiredInputs covers specification section
// 3.13.2: @deprecated must not appear on a required argument or input field,
// because a client has no way to stop supplying one.
func TestNewSchemaRejectsDeprecatedRequiredInputs(t *testing.T) {
	reqArg := []SchemaOption{
		Args[depArgs](InputField("a", func(x *depArgs, v int32) { x.A = v })),
		Query(FieldArgs("f", func(Root, depArgs) string { return "" })),
	}
	nullArg := []SchemaOption{
		Args[depNullArgs](InputField("a", func(x *depNullArgs, v *int32) { x.A = v })),
		Query(FieldArgs("f", func(Root, depNullArgs) string { return "" })),
	}
	inField := []SchemaOption{
		Input[depIn]("I", InputField("a", func(x *depIn, v int32) { x.A = v })),
		Args[depInArgs](InputField("i", func(x *depInArgs, v depIn) { x.I = v })),
		Query(FieldArgs("f", func(Root, depInArgs) string { return "" })),
	}
	cases := []struct {
		name string
		sdl  string
		opts []SchemaOption
		want string // substring of the expected error; empty means must build
	}{
		{"required argument", `type Query { f(a: Int! @deprecated): String! }`, reqArg, "Query.f(a:)"},
		{"required input field", `input I { a: Int! @deprecated } type Query { f(i: I!): String! }`, inField, "I.a"},
		{"non-null argument with a default", `type Query { f(a: Int! = 1 @deprecated): String! }`, reqArg, ""},
		{"nullable argument", `type Query { f(a: Int @deprecated): String! }`, nullArg, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(SDL(tc.sdl), tc.opts...)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("schema must build: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "@deprecated") {
				t.Fatalf("error should mention %q and @deprecated:\n%s", tc.want, msg)
			}
		})
	}
}
