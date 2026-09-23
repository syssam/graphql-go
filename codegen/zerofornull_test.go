package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An ORM emits filter inputs by the hundred with `IsNil bool` under `Boolean`.
// The engine refuses that by default, and rightly; ZeroForNullInputs is the
// one place the author says the two mean the same thing for this schema.
func TestZeroForNullInputsIsEmitted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	const sdl = `
directive @goModel(model: String) on INPUT_OBJECT
input Filter @goModel(model: "hello/ent.Filter") { isNil: Boolean }
type Doc { id: ID! }
type Query { docs(f: Filter): [Doc!]! }
`
	const source = `package ent

type Filter struct {
	IsNil bool ` + "`json:\"isNil\"`" + `
}

type Doc struct{ ID string }
`
	dir := writeEntModule(t, sdl, source)
	cfg := Config{ModelDirective: ModelDirective{Name: "goModel", Arg: "model"}}

	// Off by default: the binding is plain and the engine will refuse it,
	// which is the behaviour every existing schema keeps.
	if src := autoGenerate(t, dir, cfg); !strings.Contains(src, `graphql.Input[ent.Filter]("Filter"),`) {
		t.Errorf("ZeroForNull appeared without being asked for\n%s", src)
	}

	cfg.ZeroForNullInputs = true
	src := autoGenerate(t, dir, cfg)
	if !strings.Contains(src, `graphql.Input[ent.Filter]("Filter", graphql.ZeroForNull()),`) {
		t.Errorf("ZeroForNullInputs did not reach the emitted binding\n%s", src)
	}

	// And the whole point: with it, the generated schema actually builds.
	stub := `package graph

import (
	"context"

	"hello/ent"
)

type Stub struct{}

func (Stub) Docs(_ context.Context, a DocsArgs) ([]*ent.Doc, error) { return nil, nil }
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package graph_test

import (
	"testing"

	"hello/graph"
)

func TestSchemaBuilds(t *testing.T) {
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
