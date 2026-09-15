package buildbench

import (
	"sort"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// TestSDLParses is the gate on the generator: both engines must be handed a
// schema that is valid in the first place, or the measurement is meaningless.
func TestSDLParses(t *testing.T) {
	for _, n := range []int{1, 5, 50} {
		src := SDL(n)
		schema, err := gqlparser.LoadSchema(&ast.Source{Name: "bench.graphql", Input: src})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if got := schema.Types[EntityName(0)]; got == nil {
			t.Fatalf("n=%d: %s missing", n, EntityName(0))
		}
		if schema.Query == nil || schema.Mutation == nil {
			t.Fatalf("n=%d: missing root types", n)
		}
		// gqlparser injects __schema / __type into Query; the prelude adds
		// Query.node and Mutation.ping that the entities extend.
		if got := declaredFields(schema.Query); got != 2*n+1 {
			t.Fatalf("n=%d: Query has %d declared fields, want %d", n, got, 2*n+1)
		}
		if got := declaredFields(schema.Mutation); got != 3*n+1 {
			t.Fatalf("n=%d: Mutation has %d declared fields, want %d", n, got, 3*n+1)
		}
	}
}

func TestSDLGrowsWithN(t *testing.T) {
	small, large := SDL(10), SDL(20)
	if len(large) <= len(small) {
		t.Fatalf("SDL(20)=%d bytes is not larger than SDL(10)=%d", len(large), len(small))
	}
	if !strings.Contains(large, EntityName(19)) {
		t.Fatalf("SDL(20) missing %s", EntityName(19))
	}
}

// declaredFields counts fields the generator emitted, skipping the
// introspection fields gqlparser injects.
func declaredFields(def *ast.Definition) int {
	n := 0
	for _, f := range def.Fields {
		if !strings.HasPrefix(f.Name, "__") {
			n++
		}
	}
	return n
}

// TestSDLFilesMatchesSDL is what makes the flat and split runs comparable:
// both layouts must describe exactly the same schema.
func TestSDLFilesMatchesSDL(t *testing.T) {
	const n = 8
	files := SDLFiles(n)
	if len(files) != n+1 {
		t.Fatalf("SDLFiles(%d) returned %d files, want %d", n, len(files), n+1)
	}

	var sources []*ast.Source
	for name, body := range files {
		sources = append(sources, &ast.Source{Name: name, Input: body})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })

	split, err := gqlparser.LoadSchema(sources...)
	if err != nil {
		t.Fatalf("split schema: %v", err)
	}
	flat, err := gqlparser.LoadSchema(&ast.Source{Name: "bench.graphql", Input: SDL(n)})
	if err != nil {
		t.Fatalf("flat schema: %v", err)
	}

	if len(split.Types) != len(flat.Types) {
		t.Fatalf("split has %d types, flat has %d", len(split.Types), len(flat.Types))
	}
	for name := range flat.Types {
		if split.Types[name] == nil {
			t.Fatalf("split schema missing %s", name)
		}
	}
	if a, b := declaredFields(split.Query), declaredFields(flat.Query); a != b {
		t.Fatalf("Query fields: split %d, flat %d", a, b)
	}
	if a, b := declaredFields(split.Mutation), declaredFields(flat.Mutation); a != b {
		t.Fatalf("Mutation fields: split %d, flat %d", a, b)
	}
}
