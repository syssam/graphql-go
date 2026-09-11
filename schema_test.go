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
