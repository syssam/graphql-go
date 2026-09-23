package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goIdent upper-cases the first rune, and a leading underscore has no upper
// case, so `_lastUpdatedAt` stayed `_lastUpdatedAt`: an unexported field, which
// Input[T] skips, so the input can never bind and NewSchema refuses the schema.
// 342 fields on one real schema, and the generated code compiles perfectly --
// only building the schema finds it. A leading underscore is legal in SDL and
// conventional for a meta field, which is why one appears 342 times.
func TestGoIdentExportsALeadingUnderscore(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"_lastUpdatedAt", "XLastUpdatedAt"},
		{"__meta", "XMeta"},
		{"_", "X"},
		{"_id", "XID"},
		// Unchanged for everything that already exports.
		{"reason", "Reason"},
		{"id", "ID"},
		{"ownerId", "OwnerID"},
	} {
		if got := goIdent(c.in); got != c.want {
			t.Errorf("goIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The end of it: an SDL input carrying a leading-underscore field must produce
// a schema that builds, not merely Go that compiles.
func TestLeadingUnderscoreInputBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	const sdl = `
input VoidInput {
  _lastUpdatedAt: String!
  reason: String!
}
type Query { ok: String! }
type Mutation { void(in: VoidInput!): String! }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	if err := Generate(t.Context(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(models), "\t_lastUpdatedAt ") {
		t.Errorf("the model field is unexported, so nothing can bind it:\n%s", models)
	}

	stub := `package graph

import "context"

type Stub struct{}

func (Stub) Ok(context.Context) (string, error) { return "ok", nil }

func (Stub) Void(_ context.Context, a VoidArgs) (string, error) { return a.In.Reason, nil }
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package graph_test

import (
	"testing"

	"hello/graph"
)

func TestBuilds(t *testing.T) {
	if _, err := graph.NewSchema(graph.Stub{}); err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "build_test.go"), []byte(testSrc), 0o644); err != nil {
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
		t.Fatalf("the generated schema does not build: %v\n%s", err, out)
	}
}

// An args struct carried no `graphql` tag, so the SDL argument name was
// derived back from the Go field name -- exactly the lossy round trip the
// input-object emitter already carries a tag to avoid. It stayed hidden while
// every argument name happened to survive the trip; `_lastUpdatedAt` does not
// (it becomes XLastUpdatedAt, which derives to xLastUpdatedAt), and neither
// does ownerID.
func TestArgsStructPinsTheSDLName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	const sdl = `
type Query {
  ok(_lastUpdatedAt: String!, ownerID: String!, plain: String!): String!
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	if err := Generate(t.Context(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(src)), " ")
	for _, want := range []string{
		"XLastUpdatedAt string `graphql:\"_lastUpdatedAt\"`",
		"OwnerID string `graphql:\"ownerID\"`",
		"Plain string `graphql:\"plain\"`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("args struct does not pin the SDL name: %s\n%s", want, src)
		}
	}

	stub := `package graph

import "context"

type Stub struct{}

func (Stub) Ok(_ context.Context, a OkArgs) (string, error) { return a.Plain, nil }
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package graph_test

import (
	"testing"

	"hello/graph"
)

func TestBuilds(t *testing.T) {
	if _, err := graph.NewSchema(graph.Stub{}); err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "args_test.go"), []byte(testSrc), 0o644); err != nil {
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
		t.Fatalf("the generated schema does not build: %v\n%s", err, out)
	}
}
