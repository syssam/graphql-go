package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Binding validation needs the resolver's type, never its behaviour: Resolve
// captures the function in a closure that build never calls. So a schema can
// be checked with a zero-value resolver, before a line of it is written.
//
// Until this existed, answering "is this schema bindable?" on a real 5 503-type
// schema meant faking all 8 724 resolver methods with a 200-line AST walker.
// That is the reason it had never been asked, and why ten codegen bugs reached
// a state where the generated Go compiled and the schema could not be built.
func TestValidateSchemaNeedsNoResolvers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	// An unbound custom scalar: gqlc emits a Go type for it but cannot know
	// its wire format, so this is the error a generated schema meets first --
	// and it compiles perfectly, which is why only building the schema finds
	// it. Nothing in the generated package implements Resolver.
	const sdl = `
scalar Money
type Doc { id: ID! price: Money! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Doc struct{ ID string }
`
	dir := writeEntModule(t, sdl, source)
	autoGenerate(t, dir, Config{Models: map[string]string{"Doc": "hello/ent.Doc"}})
	schema, err := os.ReadFile(filepath.Join(dir, "graph", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), "func ValidateSchema(") {
		t.Fatalf("no ValidateSchema was generated\n%s", schema)
	}

	testSrc := `package graph_test

import (
	"strings"
	"testing"

	"hello/graph"
)

// The whole point: no Resolver implementation exists in this package.
func TestSchemaIsBindable(t *testing.T) {
	err := graph.ValidateSchema()
	if err == nil {
		t.Fatal("the unbound scalar was not reported")
	}
	if !strings.Contains(err.Error(), "Money") {
		t.Fatalf("the error does not name the unbound scalar: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "validate_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	run := exec.Command("go", "test", "-count=1", "./graph")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("ValidateSchema did not work without resolvers: %v\n%s", err, out)
	}
}

// Two or more SDL groups register each group separately rather than taking
// one interface, so ValidateSchema has a second shape and it needs its own test:
// the flat one passing says nothing about it.
func TestValidateSchemaWithGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two files, so the generator splits into groups. Money is unbound in one
	// of them, which is what ValidateSchema must report.
	for name, body := range map[string]string{
		"a.graphql": "scalar Money\ntype Doc { id: ID! price: Money! }\ntype Query { doc(id: ID!): Doc }\n",
		"b.graphql": "type Note { id: ID! body: String! }\nextend type Query { note(id: ID!): Note }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "schema", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTempModule(t, dir)
	if err := Generate(t.Context(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join(dir, "graph", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), "Bindings(nil)") {
		t.Fatalf("the grouped ValidateSchema does not register each group with a nil resolver:\n%s", schema)
	}

	testSrc := `package graph_test

import (
	"strings"
	"testing"

	"hello/graph"
)

func TestGroupedSchemaIsBindable(t *testing.T) {
	err := graph.ValidateSchema()
	if err == nil {
		t.Fatal("the unbound scalar was not reported")
	}
	if !strings.Contains(err.Error(), "Money") {
		t.Fatalf("the error does not name the unbound scalar: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "validate_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	run := exec.Command("go", "test", "-count=1", "./graph")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("grouped ValidateSchema did not work without resolvers: %v\n%s", err, out)
	}
}
