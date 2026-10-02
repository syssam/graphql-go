package codegen

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wireModule is scaffoldModule with a dependencies package beside the schema
// and Wire configured to build every group's implementation from it.
func wireModule(t *testing.T, scaffold map[string]string) (string, func() error) {
	t.Helper()
	dir, _ := scaffoldModule(t, scaffold)
	writeFiles(t, dir, map[string]string{
		"internal/deps/deps.go": "// Package deps is what every resolver is built from.\npackage deps\n\n// Deps is the application's shared dependencies.\ntype Deps struct{ Greeting string }\n",
	})
	return dir, func() error {
		return Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph",
			Scaffold: scaffold,
			Wire:     Wire{Deps: "hello/internal/deps.Deps"},
		})
	}
}

// Every group registers its bindings over its own Resolver, and something has
// to construct each one and hand the list to NewSchema. Written by hand that
// is a line per group to keep in step with the schema; left to a dependency
// container it is reflection at start-up and a missing group found when the
// process starts. gqlc knows every group and where its implementation is, so
// it writes the list, and a group with no constructor does not compile.
func TestWireBuildsEveryGroupFromOneDependenciesValue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir, generate := wireModule(t, map[string]string{
		// One implementation in its group's own package, one in a package of
		// its own, as a project is free to mix them.
		"user": "graph/user.Handler",
		"post": "internal/resolver.Posts",
	})
	if err := generate(); err != nil {
		t.Fatal(err)
	}

	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	for _, want := range []string{
		generatedMarker,
		"func Wire(d deps.Deps) []graphql.SchemaOption",
		"user.Bindings(user.NewHandler(d))",
		"post.Bindings(resolver.NewPosts(d))",
	} {
		if !strings.Contains(norm(wire), norm(want)) {
			t.Errorf("wire.go lacks %q\n%s", want, wire)
		}
	}

	// The constructor is scaffolded with the type, holding what it was given.
	user := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
	for _, want := range []string{"type Handler struct", "deps deps.Deps", "func NewHandler(d deps.Deps) *Handler"} {
		if !strings.Contains(norm(user), norm(want)) {
			t.Errorf("user.resolvers.go lacks %q\n%s", want, user)
		}
	}

	// A second run changes nothing: the constructor is there already.
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if again := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go")); again != user {
		t.Errorf("a second run changed the resolver:\n%s", again)
	}

	// And the whole thing builds a schema with nothing but the dependencies.
	writeFiles(t, dir, map[string]string{"graph/wire_use_test.go": `package graph_test

import (
	"testing"

	"hello/graph"
	"hello/internal/deps"
)

func TestWire(t *testing.T) {
	opts := graph.Wire(deps.Deps{Greeting: "hi"})
	if len(opts) != 2 {
		t.Fatalf("Wire returned %d bindings, want one per group with a Resolver", len(opts))
	}
	if _, err := graph.NewSchema(opts...); err != nil {
		t.Fatal(err)
	}
}
`})
	goBuild(t, dir)
	cmd := exec.Command("go", "test", "./graph/")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

// An implementation that exists already is the author's, and so is its
// constructor: gqlc never touches one that is there.
func TestWireLeavesAnExistingConstructorAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, generate := wireModule(t, map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"})
	const mine = `package user

import "hello/internal/deps"

// Handler is hand-written.
type Handler struct{ greeting string }

// NewHandler is hand-written too.
func NewHandler(d deps.Deps) *Handler { return &Handler{greeting: d.Greeting} }
`
	writeFiles(t, dir, map[string]string{"graph/user/handler.go": mine})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "graph", "user", "handler.go")); got != mine {
		t.Errorf("the hand-written file was changed:\n%s", got)
	}
	stubs := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
	if strings.Contains(stubs, "func NewHandler") || strings.Contains(stubs, "type Handler") {
		t.Errorf("a constructor or type that already exists was scaffolded again:\n%s", stubs)
	}
	goBuild(t, dir)
}

// A type that exists has fields gqlc knows nothing about, so it cannot write
// its constructor. It used to write one that panics, which compiled and then
// stopped the process at start-up -- the failure Wire exists to move to the
// compiler. Nothing is written now: the build fails on the missing name, and
// a note says what to write.
func TestWireWritesNoConstructorForATypeThatExists(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireModule(t, scaffold)
	writeFiles(t, dir, map[string]string{
		"graph/user/handler.go": "package user\n\n// Handler is hand-written and has no constructor.\ntype Handler struct{ greeting string }\n",
	})
	var notes []string
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph",
		Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"},
		Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
	}); err != nil {
		t.Fatal(err)
	}
	if stubs := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go")); strings.Contains(stubs, "func NewHandler") {
		t.Errorf("a constructor was written for a type gqlc did not write:\n%s", stubs)
	}
	said := strings.Join(notes, "\n")
	if !strings.Contains(said, "func NewHandler(d deps.Deps) *Handler") || !strings.Contains(said, filepath.Join("graph", "user")) {
		t.Errorf("no note says which constructor to write and where:\n%s", said)
	}
	// The other group's type is new, so it still gets its constructor.
	if post := readFile(t, filepath.Join(dir, "graph", "post", "post.resolvers.go")); !strings.Contains(post, "func NewHandler(d deps.Deps) *Handler") {
		t.Errorf("a new type lost its scaffolded constructor:\n%s", post)
	}
}

// One type may implement several groups, and it is then one value: built once
// and handed to each. A constructor call per group made as many instances as
// there were groups, each with its own caches and connections.
func TestWireBuildsASharedImplementationOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir, generate := wireModule(t, map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	if n := strings.Count(wire, "resolver.NewR(d)"); n != 1 {
		t.Errorf("the shared implementation is constructed %d times, want once:\n%s", n, wire)
	}
	// The variable holding it must not hide the package it came from.
	writeFiles(t, dir, map[string]string{"graph/wire_use_test.go": `package graph_test

import (
	"testing"

	"hello/graph"
	"hello/internal/deps"
)

func TestWire(t *testing.T) {
	opts := graph.Wire(deps.Deps{})
	if len(opts) != 2 {
		t.Fatalf("Wire returned %d bindings, want one per group", len(opts))
	}
	if _, err := graph.NewSchema(opts...); err != nil {
		t.Fatal(err)
	}
}
`})
	goBuild(t, dir)
	cmd := exec.Command("go", "test", "./graph/")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

// The dependencies package is imported into a file that already has imports,
// and its last element is no safer a name there than anywhere else.
func TestWireScaffoldsADependenciesImportThatCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}

	// A dependencies package named like a group, whose model package the
	// stubs import under the same name.
	t.Run("named like a model package", func(t *testing.T) {
		dir, _ := scaffoldModule(t, scaffold)
		writeFiles(t, dir, map[string]string{"internal/user/deps.go": "package user\n\n// Deps is the dependencies.\ntype Deps struct{}\n"})
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
			Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/user.Deps"},
		}); err != nil {
			t.Fatal(err)
		}
		goBuild(t, dir)
	})

	// A resolvers file that imports the package already, under the name the
	// author gave it or under none.
	for name, imp := range map[string]string{"unaliased": `"hello/internal/deps"`, "aliased": `app "hello/internal/deps"`} {
		t.Run("already imported "+name, func(t *testing.T) {
			dir, generate := wireModule(t, scaffold)
			qual := "deps"
			if name == "aliased" {
				qual = "app"
			}
			writeFiles(t, dir, map[string]string{
				"graph/user/user.resolvers.go": "package user\n\nimport " + imp + "\n\nvar _ " + qual + ".Deps\n",
			})
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			got := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
			if n := strings.Count(got, `"hello/internal/deps"`); n != 1 {
				t.Errorf("the dependencies package is imported %d times, want the import that was there reused:\n%s", n, got)
			}
			if !strings.Contains(got, "func NewHandler(d "+qual+".Deps) *Handler") {
				t.Errorf("the constructor does not use the name the file imports the package under:\n%s", got)
			}
			goBuild(t, dir)
		})
	}
}

// With one group everything is in the generated package, the dependencies
// type included if the author puts it there. Wire imported that package into
// itself.
func TestWireWithTheDependenciesInTheGeneratedPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema/schema.graphql": "type Thing { id: ID! }\ntype Query { thing(id: ID!): Thing }\n",
		"graph/deps.go":         "package graph\n\n// Deps is the dependencies.\ntype Deps struct{}\n",
	})
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"graph": "graph.Handler"}, Wire: Wire{Deps: "hello/graph.Deps"},
	}); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	if strings.Contains(wire, `"hello/graph"`) || !strings.Contains(wire, "func Wire(d Deps) []graphql.SchemaOption") {
		t.Errorf("wire.go should name Deps unqualified and import nothing for it:\n%s", wire)
	}
	goBuild(t, dir)
}

func TestWireRefusesWhatItCannotBuild(t *testing.T) {
	for name, tc := range map[string]struct {
		scaffold map[string]string
		wire     Wire
		want     string
	}{
		"a group with a Resolver and no implementation named": {
			map[string]string{"user": "graph/user.Handler"}, Wire{Deps: "hello/internal/deps.Deps"}, "post",
		},
		"a dependencies type with no package": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}, Wire{Deps: "Deps"}, "Wire.Deps",
		},
		"a dependencies type that is not a type": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}, Wire{Deps: "hello/internal/deps.not a type"}, "Wire.Deps",
		},
		// Every group package would import the root for Deps, and the root
		// imports every group.
		"a dependencies type in the root of a grouped schema": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}, Wire{Deps: "hello/graph.Deps"}, "import",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := scaffoldModule(t, tc.scaffold)
			err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
				Output: "graph", Package: "hello/graph",
				Scaffold: tc.scaffold, Wire: tc.wire,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
			if _, serr := os.Stat(filepath.Join(dir, "graph", "wire.go")); serr == nil {
				t.Error("wire.go was written although Wire was refused")
			}
		})
	}
}

// Off by default: nothing is written and nothing about scaffolding changes.
func TestWireIsOffByDefault(t *testing.T) {
	dir, generate := scaffoldModule(t, map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "wire.go")); err == nil {
		t.Error("wire.go was written without Wire configured")
	}
	if user := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go")); strings.Contains(user, "func NewHandler") {
		t.Errorf("a constructor was scaffolded without Wire configured:\n%s", user)
	}
}
