package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goType adds a pointer for any SDL object, interface or union. For an object
// that is right -- the Go side is a struct. For an interface or a union it is
// not: the Go side is itself an interface, and *Noder satisfies nothing. An
// unmapped abstract type became `any` and never reached the pointer, so this
// only appears once a schema maps one -- which the ORM does, with
// `interface Node @goModel(model: "pkg.Noder")`, and the engine then reports
// "Go type *pkg.Noder returned for abstract type Node is not bound to any
// object type".
func TestMappedAbstractTypeIsNotAPointer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	const sdl = `
directive @goModel(model: String) on INTERFACE | UNION | OBJECT
interface Node @goModel(model: "hello/ent.Noder") { id: ID! }
type Doc implements Node @goModel(model: "hello/ent.Doc") { id: ID! }
union Any @goModel(model: "hello/ent.Anything") = Doc
type Query { node(id: ID!): Node any: Any }
`
	const source = `package ent

type Noder interface{ IsNode() }

type Anything interface{ IsAnything() }

type Doc struct{ ID string }

func (Doc) IsNode()     {}
func (Doc) IsAnything() {}
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
		AutoBind:       nil,
	})
	for _, bad := range []string{"*ent.Noder", "*ent.Anything"} {
		if strings.Contains(src, bad) {
			t.Errorf("an abstract type was wrapped in a pointer: %s\n%s", bad, src)
		}
	}
	// The object keeps its pointer: this is about abstract types, not about
	// dropping pointers everywhere.
	if !strings.Contains(src, "*ent.Doc") {
		t.Errorf("an object type lost its pointer\n%s", src)
	}

	stub := `package graph

import (
	"context"

	"hello/ent"
)

type Stub struct{}

func (Stub) Node(_ context.Context, a NodeArgs) (ent.Noder, error) { return ent.Doc{ID: "1"}, nil }

func (Stub) Any(context.Context) (ent.Anything, error) { return ent.Doc{ID: "1"}, nil }
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
	if err := os.WriteFile(filepath.Join(dir, "graph", "abs_test.go"), []byte(testSrc), 0o644); err != nil {
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
