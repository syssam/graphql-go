package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A generated group package imports the model package for its own group and,
// where a type is mapped, the package that type comes from. Nothing stopped
// those two from having the same base name:
//
//	import (
//	    "example.com/app/billing"            // a mapped type lives here
//	    "example.com/app/graph/model/billing" // the group's generated models
//	)
//
// On a real 5 503-type schema that was 410 redeclarations and most of 1 983
// undefined symbols across 381 packages -- one bug, and the generated tree did
// not build at all.
func TestImportCollisionIsAliased(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	dir := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Two groups, so models split. The billing group has a mapped type whose
	// Go package is also called billing.
	mk("schema/billing.graphql", `
type Invoice @goModel(model: "hello/billing.Invoice") { id: ID! total: Int! }
input InvoiceFilter { minTotal: Int }
type Query { invoice(id: ID!): Invoice invoices(filter: InvoiceFilter): [Invoice!]! }
`)
	mk("schema/other.graphql", "type Note { id: ID! body: String! }\nextend type Query { note(id: ID!): Note }\n")
	mk("schema/prelude.graphql", "directive @goModel(model: String) on OBJECT | INPUT_OBJECT | SCALAR | ENUM\n")
	mk("billing/invoice.go", "package billing\n\ntype Invoice struct {\n\tID    string\n\tTotal int\n}\n")
	writeTempModule(t, dir)

	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph",
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		// The entity stores its id as a string; without AutoBind the generator
		// cannot know that, so the scalar is mapped. Otherwise this fixture
		// fails on the ID type rather than on the thing it is testing.
		Models: map[string]string{"ID": "string"},
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, out)
	}
}
