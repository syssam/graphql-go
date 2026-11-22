package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go shapes AutoBind once bound into code that does not compile, or compiles
// and serves a wrong value. Each is a field of one schema, so one build proves
// them all:
//
//   - Gen is generic and Aliased an alias of one instance: neither has a name
//     to bind, so each gets a generated model.
//   - Meth.Label is a generic method, which a generated call cannot
//     instantiate, so it falls to the Resolver.
//   - Shadow.Name() hides the embedded Base.Name, so the method answers.
//   - Page is named by Models as an instance, and its fields are read from
//     that instance: v.URL, not v.Url.
//   - Nums.Big is a uint64, which int cannot hold, so it falls to the Resolver;
//     Nums.Small is a uint32, which it can.
func TestAutoBindGoShapesCompile(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping compile")
	}
	const sdl = `type Query { g: Gen a: Aliased m: Meth s: Shadow p: Page n: Nums }
type Gen { total: Int! }
type Aliased { v: String! }
type Meth { label: String! }
type Shadow { id: ID! name: String! }
type Page { url: String! }
type Nums { big: Int! small: Int! }
`
	const src = `package ent

type Gen[T any] struct{ Total int; Items []T }

type Box[T any] struct{ V T }

type Aliased = Box[string]

type Meth struct{}

func (m *Meth) Label[T any]() string { return "" }

type Base struct{ Name string }

type Shadow struct {
	Base
	ID string
}

func (s *Shadow) Name() string { return "shadow" }

type Page[T any] struct {
	URL   string
	Items []T
}

type Nums struct {
	Big   uint64
	Small uint32
}
`
	dir := writeEntModule(t, sdl, src)
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
		AutoBind: []string{"./ent"},
		Models:   map[string]string{"Page": "hello/ent.Page[int]"},
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	gen, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	src2 := string(gen)
	for _, c := range []struct {
		want    bool
		snippet string
		why     string
	}{
		{false, "ent.Gen", "a generic type was bound"},
		{false, "ent.Box", "an alias of a generic instance was bound"},
		{false, "v.Label(", "a generic method was called"},
		{true, "v.Name()", "the method hiding Base.Name does not answer"},
		{true, "v.URL", "the Models instance's fields were not read"},
		{false, "int(v.Big)", "a uint64 was converted to int"},
		{true, "int(v.Small)", "a uint32 was not converted to int"},
	} {
		if strings.Contains(src2, c.snippet) != c.want {
			t.Errorf("%s (%q)", c.why, c.snippet)
		}
	}
	goBuild(t, dir)
}

// A Models entry behind * or [] for an object, input or enum is refused: the
// generator writes the pointer itself, and *ent.Product became
// Object[*ent.Product] over func(v **ent.Product). A scalar may be either,
// which is what they are for.
func TestModelsRefuseIndirectionOnNonScalars(t *testing.T) {
	t.Parallel()
	sdl := "scalar Blob\ntype Query { p: Product e: E i(x: In): Int }\ntype Product { url: String! }\nenum E { A }\ninput In { a: Int }\n"
	for name, expr := range map[string]string{
		"Product": "*hello/ent.Product",
		"E":       "[]hello/ent.E",
		"In":      "*hello/ent.In",
	} {
		_, err := generateWith(t, sdl, Config{Models: map[string]string{name: expr}})
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "scalar") {
			t.Errorf("Models[%s] = %s: err = %v, want a refusal naming it", name, expr, err)
		}
	}
	if _, err := generateWith(t, sdl, Config{Models: map[string]string{"Blob": "[]byte"}}); err != nil {
		t.Errorf("a slice scalar was refused: %v", err)
	}
}
