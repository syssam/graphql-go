package graphql

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vektah/gqlparser/v2/ast"
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

// SDLBytes is what a caller holding an embed.FS file or a read schema reaches
// for, and nothing here had ever called it.
func TestSDLBytes(t *testing.T) {
	type A struct{ ID string }
	s, err := NewSchema(SDLBytes([]byte(`type Query { a: A } type A { id: ID! }`)),
		Object[Root]("Query", Field("a", func(Root) *A { return nil })),
		Object[A]("A", Field("id", func(a *A) string { return a.ID })),
	)
	if err != nil {
		t.Fatal(err)
	}
	if s.AST().Types["A"] == nil {
		t.Fatal("type A missing from a schema built from bytes")
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

// gqlparser's formatter never writes ast.Schema.Description, so PrintSDL lost
// what introspection reports as __schema.description. With the default root
// names the formatter also writes no schema definition at all, which leaves
// the description nowhere to go; both shapes are covered for that reason.
func TestPrintSDLKeepsSchemaDescription(t *testing.T) {
	for _, tc := range []struct{ name, sdl, query string }{
		{"default root name", `"""Root schema"""
schema { query: Query }
type Query { a: Int }`, "Query"},
		{"custom root name", `"""Root schema"""
schema { query: Q }
type Q { a: Int }`, "Q"},
		{"schema directive and mutation root", `directive @meta on SCHEMA
"""Root schema"""
schema @meta { query: Query mutation: M }
type Query { a: Int }
type M { a: Int }`, "Query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bind := Options(Object[Root](tc.query, Field("a", func(Root) int { return 1 })))
			if strings.Contains(tc.sdl, "mutation: M") {
				bind = Options(bind, Object[Root]("M", Field("a", func(Root) int { return 1 })))
			}
			s, err := NewSchema(SDL(tc.sdl), bind)
			if err != nil {
				t.Fatal(err)
			}
			printed := PrintSDL(s)
			again, err := NewSchema(SDL(printed), bind)
			if err != nil {
				t.Fatalf("printed SDL does not reload: %v\n%s", err, printed)
			}
			if got := again.AST().Description; got != "Root schema" {
				t.Fatalf("description after round trip = %q, want %q\n%s", got, "Root schema", printed)
			}
			if got := again.AST().Query.Name; got != tc.query {
				t.Fatalf("query root after round trip = %q, want %q\n%s", got, tc.query, printed)
			}
			if (s.AST().Mutation == nil) != (again.AST().Mutation == nil) {
				t.Fatalf("mutation root lost in round trip\n%s", printed)
			}
			if got, want := len(again.AST().SchemaDirectives), len(s.AST().SchemaDirectives); got != want {
				t.Fatalf("schema directives after round trip = %d, want %d\n%s", got, want, printed)
			}
		})
	}
}

// gqlparser's formatter writes every description as a block string without
// escaping, so a description holding `"""` closed its own block early and the
// printed SDL did not load. Every place a description can sit is covered,
// because each is written by a different formatter path.
func TestPrintSDLEscapesTripleQuotes(t *testing.T) {
	type en int
	type in struct{ V *string }
	type args struct{ A *in }
	const q = `\"""`
	sdl := `"""schema ` + q + `"""
schema { query: Query }
"""directive ` + q + `"""
directive @d("""directive arg ` + q + `""" x: Int) on FIELD_DEFINITION
"""type ` + q + `"""
type Query {
  """field ` + q + `"""
  f("""arg ` + q + `""" a: In): E
}
"""enum ` + q + `"""
enum E { """value ` + q + `""" V }
"""input ` + q + `"""
input In { """input field ` + q + `""" v: String }`
	opts := Options(
		Enum("E", map[en]string{0: "V"}),
		Input[in]("In"),
		Args[args](),
		Query(FieldArgs("f", func(Root, args) en { return 0 })),
	)
	s, err := NewSchema(SDL(sdl), opts)
	if err != nil {
		t.Fatal(err)
	}
	printed := PrintSDL(s)
	again, err := NewSchema(SDL(printed), opts)
	if err != nil {
		t.Fatalf("printed SDL does not reload: %v\n%s", err, printed)
	}
	want, got := descriptions(s.AST()), descriptions(again.AST())
	if len(want) != 10 {
		t.Fatalf("fixture has %d descriptions, want 10: %v", len(want), want)
	}
	for where, d := range want {
		if !strings.Contains(d, `"""`) {
			t.Fatalf("fixture description at %s = %q, want it to hold a triple quote", where, d)
		}
		if got[where] != d {
			t.Errorf("description at %s after round trip = %q, want %q", where, got[where], d)
		}
	}
}

// descriptions collects every non-built-in description in s, keyed by where
// it sits.
func descriptions(s *ast.Schema) map[string]string {
	out := map[string]string{}
	add := func(where, d string) {
		if d != "" {
			out[where] = d
		}
	}
	add("schema", s.Description)
	for _, d := range s.Directives {
		if d.Position != nil && d.Position.Src.BuiltIn {
			continue
		}
		add("@"+d.Name, d.Description)
		for _, a := range d.Arguments {
			add("@"+d.Name+"("+a.Name+")", a.Description)
		}
	}
	for _, t := range s.Types {
		if t.BuiltIn {
			continue
		}
		add(t.Name, t.Description)
		for _, f := range t.Fields {
			add(t.Name+"."+f.Name, f.Description)
			for _, a := range f.Arguments {
				add(t.Name+"."+f.Name+"("+a.Name+")", a.Description)
			}
		}
		for _, v := range t.EnumValues {
			add(t.Name+"."+v.Name, v.Description)
		}
	}
	return out
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
