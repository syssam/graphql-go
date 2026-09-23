package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The import block used to be decided by searching the generated text for
// "<qualifier>.", which cannot tell code from a comment. Every custom scalar
// carries "bind it with graphql.Scalar in NewSchema options" above it, so a
// model package holding only scalars imported the engine and did not use it --
// 1 006 packages on one real schema, each a compile error in a file marked DO
// NOT EDIT.
func TestNoImportIsWrittenForACommentAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	dir := t.TempDir()
	const sdl = `
scalar Any
scalar Date
type Doc { id: ID! }
type Query { doc(id: ID!): Doc }
`
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	if err := Generate(t.Context(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		// Two groups, so models are split and the scalar-only package is
		// written on its own.
		GroupFunc: func(typeName, sdlFile string) string {
			switch typeName {
			case "Any", "Date":
				return "scalars"
			}
			return "docs"
		},
	}); err != nil {
		t.Fatal(err)
	}

	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "scalars", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(models), "graphql.Scalar in NewSchema") {
		t.Fatalf("the fixture no longer produces the comment this is about:\n%s", models)
	}
	if strings.Contains(string(models), `"github.com/syssam/graphql-go"`) {
		t.Errorf("the engine was imported for a comment alone:\n%s", models)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// The fallback matters more than the happy path: if the body ever stops
// parsing, a missing import is a compile error in generated code, where a
// spurious one is merely noise. Nothing in the suite produces an unparsable
// body, so this asks the function directly.
func TestUsedQualifiersFallsBackToASearch(t *testing.T) {
	broken := "func Bindings() { graphql.Field(\n" // deliberately unbalanced
	used := usedQualifiers(broken)
	if !used("graphql") {
		t.Error("an unparsable body lost its import")
	}
	if used("context") {
		t.Error("the fallback claimed an import nothing mentions")
	}

	// And the parse, which is the path everything else takes: a qualifier
	// that appears only in a comment or a string is not a use.
	ok := usedQualifiers("// see graphql.Scalar\nvar s = \"time.Time\"\nvar x = ent.Doc{}\n")
	if ok("graphql") {
		t.Error("a comment counted as a use")
	}
	if ok("time") {
		t.Error("a string literal counted as a use")
	}
	if !ok("ent") {
		t.Error("a real use was missed")
	}
}
