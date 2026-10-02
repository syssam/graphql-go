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

// wireFieldModule is scaffoldModule with a service per group and a
// dependencies struct holding them, each in a field named after its group.
func wireFieldModule(t *testing.T, scaffold map[string]string, deps string) (string, func() error) {
	t.Helper()
	dir, _ := scaffoldModule(t, scaffold)
	writeFiles(t, dir, map[string]string{
		"internal/svc/user/user.go": "// Package user is the user service.\npackage user\n\n// Service is the user service.\ntype Service struct{ Greeting string }\n",
		"internal/svc/post/post.go": "// Package post is the post service.\npackage post\n\n// Service is the post service.\ntype Service struct{ Greeting string }\n",
		"internal/deps/deps.go":     deps,
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

const wireFieldDeps = `// Package deps is what every resolver is built from.
package deps

import (
	"hello/internal/svc/post"
	usersvc "hello/internal/svc/user"
)

// Deps is the application's shared dependencies.
type Deps struct {
	User *usersvc.Service
	Post post.Service
}
`

// A constructor taking the whole dependencies struct makes its package import
// everything that struct names. With a service per group, every group's
// package then recompiles when any one service changes: 305 packages and 70 s
// of CPU for one service's new method, at 300 groups, against 6 packages when
// each group's package imports its own service alone. So a field of the
// struct named after a group is what that group's constructor is given.
func TestWireHandsAGroupTheFieldNamedAfterIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir, generate := wireFieldModule(t, map[string]string{
		"user": "graph/user.Handler",
		"post": "internal/resolver.Posts",
	}, wireFieldDeps)
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	for _, want := range []string{
		"func Wire(d deps.Deps) []graphql.SchemaOption",
		"user.Bindings(user.NewHandler(d.User))",
		"post.Bindings(resolver.NewPosts(d.Post))",
	} {
		if !strings.Contains(norm(wire), norm(want)) {
			t.Errorf("wire.go lacks %q\n%s", want, wire)
		}
	}

	// The constructor is scaffolded over the field's type, under the name the
	// dependencies file imports its package by, and the group's package does
	// not import the dependencies package at all: that import is the cost.
	user := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
	for _, want := range []string{"dep *usersvc.Service", "func NewHandler(dep *usersvc.Service) *Handler", `usersvc "hello/internal/svc/user"`} {
		if !strings.Contains(norm(user), norm(want)) {
			t.Errorf("user.resolvers.go lacks %q\n%s", want, user)
		}
	}
	if strings.Contains(user, "hello/internal/deps") {
		t.Errorf("the group's package imports the whole dependencies package:\n%s", user)
	}
	// A value field, from an import the dependencies file did not alias. The
	// stubs already import the post group's models as post, so the service
	// package takes the next name.
	posts := readFile(t, filepath.Join(dir, "internal", "resolver", "post.resolvers.go"))
	if !strings.Contains(norm(posts), norm("func NewPosts(dep post2.Service) *Posts")) {
		t.Errorf("post.resolvers.go lacks a constructor over the field's type:\n%s", posts)
	}

	// A second run changes nothing.
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if again := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go")); again != user {
		t.Errorf("a second run changed the resolver:\n%s", again)
	}

	writeFiles(t, dir, map[string]string{"graph/wire_use_test.go": `package graph_test

import (
	"testing"

	"hello/graph"
	"hello/internal/deps"
	"hello/internal/svc/user"
)

func TestWire(t *testing.T) {
	opts := graph.Wire(deps.Deps{User: &user.Service{Greeting: "hi"}})
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

// When a group gets the whole value instead, and why.
func TestWireHandsOverTheWholeValueWhereNoFieldFits(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct {
		scaffold map[string]string
		deps     string
		want     []string
	}{
		// Another package cannot read it.
		"the field is unexported": {
			both, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tuser *usersvc.Service", 1),
			[]string{"user.Bindings(user.NewHandler(d))", "post.Bindings(post.NewHandler(d.Post))"},
		},
		// One type serving two groups has one constructor, which cannot take
		// two different fields; it is still built once.
		"one type implements both groups": {
			map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}, wireFieldDeps,
			[]string{"r := resolver.NewR(d)", "user.Bindings(r)", "post.Bindings(r)"},
		},
		// A group's name is matched whatever its case and punctuation, as
		// OrderItem is the orderitem group's.
		"the field is spelled in another case": {
			both, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tUSER *usersvc.Service", 1),
			[]string{"user.Bindings(user.NewHandler(d.USER))"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wireFieldModule(t, c.scaffold, c.deps)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
			for _, want := range c.want {
				if !strings.Contains(norm(wire), norm(want)) {
					t.Errorf("wire.go lacks %q\n%s", want, wire)
				}
			}
			// The constructor scaffolded has to take what Wire passes it.
			if _, shared := c.scaffold["user"]; shared && c.scaffold["user"] == c.scaffold["post"] {
				impl := readFile(t, filepath.Join(dir, "internal", "resolver", "post.resolvers.go"))
				if !strings.Contains(norm(impl), norm("func NewR(d deps.Deps) *R")) {
					t.Errorf("the shared type's constructor does not take the whole value:\n%s", impl)
				}
			}
		})
	}
}

// A group is named after its SDL file, and line_item.graphql is the lineitem
// group; the Go field is LineItem. Both are compared with case and
// punctuation taken out.
func TestWireMatchesAGroupToItsFieldWhateverThePunctuation(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema/query.graphql":     "type Query { ping: Boolean }\n",
		"schema/line_item.graphql": "type LineItem { id: ID! }\nextend type Query { lineItems: [LineItem!]! }\n",
		"deps/deps.go":             "package deps\n\n// Deps is the dependencies.\ntype Deps struct {\n\tLineItem *Store\n}\n\n// Store is a store.\ntype Store struct{}\n",
	})
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"lineitem": "graph/lineitem.Handler", "query": "graph/query.Handler"},
		Wire:     Wire{Deps: "hello/deps.Deps"},
	}); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	for _, want := range []string{"NewHandler(d.LineItem))", "query.Bindings(query.NewHandler(d))"} {
		if !strings.Contains(norm(wire), norm(want)) {
			t.Errorf("wire.go lacks %q\n%s", want, wire)
		}
	}
}

// A field's type is written into another package, so what it names has to be
// named from there: a type of the dependencies package itself gains that
// package's qualifier, and a qualifier already taken in the stub file gives way.
func TestWireWritesAFieldsTypeAsTheGroupsPackageSeesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct{ deps, want string }{
		"a type declared beside the struct": {
			"package deps\n\n// Store is a store.\ntype Store struct{}\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *Store\n\tPost map[string][]*Store\n}\n",
			"func NewHandler(dep *deps.Store) *Handler",
		},
		// The stubs import the user group's model package as user.
		"a package named like the group's models": {
			"package deps\n\nimport \"hello/internal/svc/user\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *user.Service\n\tPost func(name string) (*user.Service, error)\n}\n",
			"func NewHandler(dep *user2.Service) *Handler",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wireFieldModule(t, both, c.deps)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			got := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
			if !strings.Contains(norm(got), norm(c.want)) {
				t.Errorf("user.resolvers.go lacks %q\n%s", c.want, got)
			}
			goBuild(t, dir)
		})
	}
}

// Where other groups are handed a field and a new one is not, the likeliest
// reason is a field not added yet. The constructor is scaffolded over the
// whole value, which works and costs that package its build isolation, so
// the run that writes it says so once.
func TestWireSaysWhenANewGroupGetsTheWholeValueBesideOthersThatDoNot(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireFieldModule(t, scaffold, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service\n", "", 1))
	var notes []string
	generate := func() error {
		return Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
			Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"},
			Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
		})
	}
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	said := strings.Join(notes, "\n")
	if !strings.Contains(said, "user") || !strings.Contains(said, "Deps") || !strings.Contains(said, "field") {
		t.Errorf("no note says the user group has no field of its own:\n%s", said)
	}
	if strings.Contains(said, "post group") {
		t.Errorf("the post group has its field and was reported anyway:\n%s", said)
	}
	// Once the constructor exists there is nothing to decide, and no note.
	notes = nil
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if said := strings.Join(notes, "\n"); strings.Contains(said, "field") {
		t.Errorf("the note was repeated on a run that scaffolded nothing:\n%s", said)
	}
}

// Everything below was found by a review that ran the generator on inputs the
// tests above did not have. Each case generates and then compiles: what Wire
// passes and what the constructor takes have to be the same type, whatever
// the dependencies struct looks like.
func TestWireFieldsThatCannotBeHandedOverFallBackAndStillCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	const whole = "user.Bindings(user.NewHandler(d))"
	const own = "user.Bindings(user.NewHandler(d.User))"
	for name, c := range map[string]struct {
		deps  string
		files map[string]string
		want  string
	}{
		// Another package cannot name it.
		"an unexported type beside the struct": {
			deps: "package deps\n\ntype store struct{}\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *store\n\tPost *store\n}\n",
			want: whole,
		},
		// Written out again in another package it is a different type: the
		// tag is part of it, and an unexported member belongs to its package.
		"a struct type written inline": {
			deps: "package deps\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser struct {\n\t\tName string `json:\"name\"`\n\t}\n\tPost struct{ n int }\n}\n",
			want: whole,
		},
		"an interface type written inline": {
			deps: "package deps\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser interface{ get() int }\n\tPost any\n}\n",
			want: whole,
		},
		// A bare name may come from the dot-import as well as from beside the
		// struct, and nothing here can tell which.
		"a type from a dot-import": {
			deps: "package deps\n\nimport . \"hello/internal/svc/user\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *Service\n\tPost int\n}\n",
			want: whole,
		},
		// Two fields that are one name once case and punctuation are gone.
		"two fields with one folded name": {
			deps: strings.Replace(wireFieldDeps, "\tUser *usersvc.Service\n", "\tUser *usersvc.Service\n\tU_ser int\n", 1),
			want: whole,
		},
		// The struct is the one the build uses: not one in a file no build
		// includes, and not a type declared inside a function.
		"a file the build leaves out declares the type too": {
			deps: wireFieldDeps,
			files: map[string]string{
				"internal/deps/a_gen.go": "//go:build ignore\n\npackage main\n\ntype Deps struct {\n\tUser int\n\tPost string\n}\n",
			},
			want: own,
		},
		"a function declares a type of the same name": {
			deps: wireFieldDeps,
			files: map[string]string{
				"internal/deps/a_helper.go": "package deps\n\nfunc helper() any {\n\ttype Deps struct{ User int }\n\treturn Deps{}\n}\n\nvar _ = helper\n",
			},
			want: own,
		},
		// An import with no name is known by its package's name, which is
		// read from the package rather than guessed from its directory.
		"a package not named after its directory": {
			deps: "package deps\n\nimport \"hello/internal/svc/acct\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *user.Service\n\tPost int\n}\n",
			files: map[string]string{
				"internal/svc/acct/acct.go": "// Package user is named unlike its directory.\npackage user\n\n// Service is a service.\ntype Service struct{}\n",
			},
			want: own,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wireFieldModule(t, both, c.deps)
			writeFiles(t, dir, c.files)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm(c.want)) {
				t.Errorf("wire.go lacks %q\n%s", c.want, wire)
			}
			goBuild(t, dir)
		})
	}
}

// Two files the build chooses between each declare the struct, and nothing
// here evaluates build constraints, so neither is trusted.
func TestWireReadsNoFieldsFromAStructDeclaredTwice(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, generate := wireFieldModule(t, both, "//go:build !linux\n\n"+wireFieldDeps)
	writeFiles(t, dir, map[string]string{
		"internal/deps/deps_linux.go": "package deps\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser int\n\tPost int\n}\n",
	})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	if strings.Contains(wire, "d.User") || strings.Contains(wire, "d.Post") {
		t.Errorf("a field was read from one of two declarations:\n%s", wire)
	}
}

// A constructor that exists says what it takes, and Wire passes that. Going
// by the struct alone broke every constructor written to take the whole value
// the moment a field named after its group appeared, in a file marked DO NOT
// EDIT, and left a group that wants the whole value no way to ask for it.
func TestWirePassesWhatAnExistingConstructorTakes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, generate := wireFieldModule(t, both, wireFieldDeps)
	writeFiles(t, dir, map[string]string{
		"graph/user/handler.go": `package user

import app "hello/internal/deps"

// Handler needs more than its own service.
type Handler struct{ all app.Deps }

// NewHandler takes everything, on purpose.
func NewHandler(d app.Deps) *Handler { return &Handler{all: d} }
`,
	})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	for _, want := range []string{"user.Bindings(user.NewHandler(d))", "post.Bindings(post.NewHandler(d.Post))"} {
		if !strings.Contains(norm(wire), norm(want)) {
			t.Errorf("wire.go lacks %q\n%s", want, wire)
		}
	}
	goBuild(t, dir)
}

// A name the stub file imports a package under must be free in the whole
// package, variables and constants included: the resolver package's own
// logger was called log, and so was the package a field's type came from.
func TestWireImportNamesAvoidThePackagesVariables(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	scaffold := map[string]string{"user": "internal/resolver.Users", "post": "internal/resolver.Posts"}
	dir, generate := wireFieldModule(t, scaffold, "package deps\n\nimport (\n\t\"log\"\n\n\tusersvc \"hello/internal/svc/user\"\n)\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *usersvc.Service\n\tPost *log.Logger\n}\n")
	writeFiles(t, dir, map[string]string{
		"internal/resolver/log.go": "package resolver\n\nimport \"log/slog\"\n\nvar log = slog.Default()\n\nconst usersvc = \"users\"\n\nvar _, _ = log, usersvc\n",
	})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	goBuild(t, dir)
}

// The notes say what is so, to the run that can act on it, and nothing else.
func TestWireNotesAreTrueAndOnlyWhereTheyApply(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct {
		scaffold map[string]string
		deps     string
		want     string // a note must contain this; empty means no wire note at all
	}{
		// The fields are there; one type serving both groups is why it gets
		// the whole value, and that needs no advice.
		"one type for both groups": {map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}, wireFieldDeps, ""},
		// No group is handed a field, so none is the odd one out.
		"a struct with no field for any group": {both, "package deps\n\n// Deps is the dependencies.\ntype Deps struct{ Greeting string }\n", ""},
		// A field nothing is named after is not this run's business.
		"an unplaceable field no group is named after": {both, strings.Replace(wireFieldDeps, "type Deps struct {", "type Deps struct {\n\tCache struct{ n int }", 1), ""},
		// The group's own field is there and cannot be handed over: say why.
		"the group's field has an inline type": {both, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tUser struct{ n int }", 1), "User"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := wireFieldModule(t, c.scaffold, c.deps)
			var notes []string
			if err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
				Scaffold: c.scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"},
				Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
			}); err != nil {
				t.Fatal(err)
			}
			var wire []string
			for _, n := range notes {
				if strings.HasPrefix(n, "wire:") {
					wire = append(wire, n)
				}
			}
			switch {
			case c.want == "" && len(wire) > 0:
				t.Errorf("a note nobody can act on:\n%s", strings.Join(wire, "\n"))
			case c.want != "" && !strings.Contains(strings.Join(wire, "\n"), c.want):
				t.Errorf("no note mentions %s:\n%s", c.want, strings.Join(wire, "\n"))
			}
		})
	}
}

// A third review, again by running the generator. What an existing
// constructor takes is read for what it is, not for what it ends in.
func TestWireReadsAnExistingConstructorExactly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	handler := func(imports, param string) string {
		return "package user\n\n" + imports + "\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler is hand-written.\nfunc NewHandler(" + param + ") *Handler { return &Handler{} }\n"
	}
	for name, c := range map[string]struct {
		deps    string
		files   map[string]string
		want    string
		twice   bool
		noNotes bool
	}{
		// Each service calls its own dependencies Deps, as the application
		// calls its own. Taken for the whole value by its last element, the
		// scaffolded constructor was passed d on the second run.
		"a field type with the dependencies type's name": {
			deps: "package deps\n\nimport (\n\t\"hello/internal/svc/post\"\n\t\"hello/internal/svc/user\"\n)\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser user.Deps\n\tPost post.Deps\n}\n",
			files: map[string]string{
				"internal/svc/user/deps.go": "package user\n\n// Deps is what the user service needs.\ntype Deps struct{}\n",
				"internal/svc/post/deps.go": "package post\n\n// Deps is what the post service needs.\ntype Deps struct{}\n",
			},
			want: "user.Bindings(user.NewHandler(d.User))", twice: true,
		},
		// Nothing has to be written for a constructor that exists, so whether
		// the field's type could be written elsewhere does not come into it.
		"a constructor taking a field gqlc could not have written": {
			deps: "package deps\n\ntype store struct{}\n\n// Get is a method.\nfunc (store) Get() string { return \"\" }\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser store\n\tPost int\n}\n",
			files: map[string]string{
				"graph/user/handler.go": handler("type getter interface{ Get() string }\n", "g getter"),
			},
			want: "user.Bindings(user.NewHandler(d.User))",
		},
		"a constructor taking a pointer to the whole value": {
			deps: wireFieldDeps,
			files: map[string]string{
				"graph/user/handler.go": handler("import \"hello/internal/deps\"\n", "d *deps.Deps"),
			},
			want: "user.Bindings(user.NewHandler(&d))",
		},
		// A constructor is a name in the package, however it is declared.
		"a constructor declared as a variable": {
			deps: wireFieldDeps,
			files: map[string]string{
				"graph/user/handler.go": "package user\n\nimport \"hello/internal/deps\"\n\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler is a variable.\nvar NewHandler = func(d deps.Deps) *Handler { return &Handler{} }\n",
			},
			want: "user.Bindings(user.NewHandler(d))", noNotes: true,
		},
		// The group's package is outside the tree that may import it.
		"a field type from an internal package under the dependencies": {
			deps: "package deps\n\nimport \"hello/internal/deps/internal/store\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *store.Store\n\tPost int\n}\n",
			files: map[string]string{
				"internal/deps/internal/store/store.go": "// Package store is internal to deps.\npackage store\n\n// Store is a store.\ntype Store struct{}\n",
			},
			want: "user.Bindings(user.NewHandler(d))",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := wireFieldModule(t, both, c.deps)
			writeFiles(t, dir, c.files)
			var notes []string
			generate := func() error {
				return Generate(context.Background(), Config{
					Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
					Scaffold: both, Wire: Wire{Deps: "hello/internal/deps.Deps"},
					Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
				})
			}
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			if c.twice {
				if err := generate(); err != nil {
					t.Fatal(err)
				}
			}
			if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm(c.want)) {
				t.Errorf("wire.go lacks %q\n%s", c.want, wire)
			}
			if said := strings.Join(notes, "\n"); c.noNotes && strings.Contains(said, "has no NewHandler") {
				t.Errorf("a constructor that is there was reported missing:\n%s", said)
			}
			goBuild(t, dir)
		})
	}
}

// What Wire cannot make compile is refused when it is generated.
func TestWireRefusesWhatWouldNotCompile(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct {
		scaffold map[string]string
		deps     string
		want     string
	}{
		// There is no Deps to take: only Deps[T], and the configuration has
		// nowhere to say which.
		"a generic dependencies type": {both, "package deps\n\n// Deps is generic.\ntype Deps[T any] struct{ User T }\n", "generic"},
		// Both are built by a function called NewHandler, which returns one.
		"two types whose constructors have one name": {
			map[string]string{"user": "internal/resolver.Handler", "post": "internal/resolver.handler"}, wireFieldDeps, "NewHandler",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := wireFieldModule(t, c.scaffold, c.deps)
			err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
				Scaffold: c.scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"},
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, c.want)
			}
		})
	}
}

// One directory spelled two ways. Go will not import two paths that differ
// only in letter case into one package, so wire.go can name the directory
// only once: by its first spelling where the filesystem says the two are one
// directory, and not at all -- refused, saying why -- where they are not, or
// where nothing exists yet to ask.
func TestWireNamesADirectorySpelledTwoWaysOnce(t *testing.T) {
	scaffold := map[string]string{"user": "internal/Resolver.R", "post": "internal/resolver.R"}
	dir, generate := wireModule(t, scaffold)
	if err := os.MkdirAll(filepath.Join(dir, "internal", "resolver"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !caseInsensitive(t, dir) {
		// Two directories here, and Go cannot import both.
		err := generate()
		if err == nil || !strings.Contains(err.Error(), "letter case") {
			t.Fatalf("err = %v, want the two spellings refused", err)
		}
		return
	}
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	if n := strings.Count(strings.ToLower(wire), `"hello/internal/resolver"`); n != 1 {
		t.Errorf("the directory is imported %d times:\n%s", n, wire)
	}
	if n := strings.Count(wire, ".NewR(d)"); n != 1 {
		t.Errorf("the one type is constructed %d times, want once:\n%s", n, wire)
	}
	if !testing.Short() {
		goBuild(t, dir)
	}
}

// Nothing exists yet, so the filesystem cannot say whether the two spellings
// are one directory; guessing would write a file that compiles on one machine
// and not on another.
func TestWireRefusesTwoSpellingsOfADirectoryNotYetThere(t *testing.T) {
	_, generate := wireModule(t, map[string]string{"user": "internal/Resolver.R", "post": "internal/resolver.R"})
	if err := generate(); err == nil || !strings.Contains(err.Error(), "letter case") {
		t.Fatalf("err = %v, want the two spellings refused", err)
	}
}

// A constructor in a test file is not in the build Wire is part of. Writing
// another beside it compiled under go build and was a redeclaration under go
// vet and go test; nothing is written, and the note says where it is.
func TestWireDoesNotScaffoldOverAConstructorInATestFile(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireModule(t, scaffold)
	writeFiles(t, dir, map[string]string{
		"graph/user/handler_test.go": "package user\n\nimport \"hello/internal/deps\"\n\nfunc NewHandler(d deps.Deps) *Handler { return &Handler{} }\n",
	})
	var notes []string
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"},
		Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
	}); err != nil {
		t.Fatal(err)
	}
	if stubs := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go")); strings.Contains(stubs, "func NewHandler") {
		t.Errorf("a second NewHandler was written beside the one in the test file:\n%s", stubs)
	}
	if said := strings.Join(notes, "\n"); !strings.Contains(said, "handler_test.go") {
		t.Errorf("no note says the constructor is in a test file:\n%s", said)
	}
}

// A fourth review, of the fixes above. A constructor that takes the whole
// value has to be recognised however the file spells that, because the price
// of missing it is a wire.go that passes it a field.
func TestWireRecognisesTheWholeValueHoweverItIsSpelled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "internal/resolver.Posts"}
	posts := func(imports, extra, ctor string) map[string]string {
		return map[string]string{
			"internal/resolver/posts.go": "package resolver\n\n" + imports + "\n// Posts is hand-written.\ntype Posts struct{}\n\n" + extra + ctor + "\n",
		}
	}
	for name, c := range map[string]struct {
		files map[string]string
		deps  map[string]string
		want  string
	}{
		"through a dot-import": {
			files: posts("import . \"hello/internal/deps\"\n", "", "func NewPosts(d Deps) *Posts { return &Posts{} }"),
			want:  "post.Bindings(resolver.NewPosts(d))",
		},
		"through an alias declared in the package": {
			files: posts("import \"hello/internal/deps\"\n", "// Deps is short for the application's.\ntype Deps = deps.Deps\n\n", "func NewPosts(d Deps) *Posts { return &Posts{} }"),
			want:  "post.Bindings(resolver.NewPosts(d))",
		},
		// A generator kept beside the package, under a build tag, in package
		// main: the package's name is not that file's.
		"from a package with a tagged main file beside it": {
			files: posts("import \"hello/internal/deps\"\n", "", "func NewPosts(d deps.Deps) *Posts { return &Posts{} }"),
			deps:  map[string]string{"internal/deps/a_gen.go": "//go:build generate\n\npackage main\n\nfunc main() {}\n"},
			want:  "post.Bindings(resolver.NewPosts(d))",
		},
		"declared as a variable with no value yet": {
			files: posts("import \"hello/internal/deps\"\n", "", "var NewPosts func(d deps.Deps) *Posts\n\nfunc init() { NewPosts = func(deps.Deps) *Posts { return &Posts{} } }"),
			want:  "post.Bindings(resolver.NewPosts(d))",
		},
		"as a pointer, through a dot-import": {
			files: posts("import . \"hello/internal/deps\"\n", "", "func NewPosts(d *Deps) *Posts { return &Posts{} }"),
			want:  "post.Bindings(resolver.NewPosts(&d))",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wireFieldModule(t, scaffold, wireFieldDeps)
			writeFiles(t, dir, c.files)
			writeFiles(t, dir, c.deps)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm(c.want)) {
				t.Errorf("wire.go lacks %q\n%s", c.want, wire)
			}
			goBuild(t, dir)
		})
	}
}

// A plain declaration is the type; a generic one of the same name under a
// build constraint beside it is an alternative this run cannot evaluate, and
// no reason to refuse.
func TestWireTakesThePlainDeclarationOverAGenericAlternative(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, generate := wireFieldModule(t, both, wireFieldDeps)
	writeFiles(t, dir, map[string]string{
		"internal/deps/alt.go": "//go:build never\n\npackage deps\n\n// Deps is the generic alternative.\ntype Deps[T any] struct{ V T }\n",
	})
	if err := generate(); err != nil {
		t.Fatalf("refused, though the type a build uses is not generic: %v", err)
	}
	if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm("user.NewHandler(d.User)")) {
		t.Errorf("the plain declaration's fields were not read:\n%s", wire)
	}
}

// wire.go imports each group's own package and the dependencies package as
// well as the scaffold targets, so a target is compared with those too: a
// spelling of the group's package in another letter case is the same
// collision, and it was found only on the second run.
func TestWireComparesATargetWithThePackagesItAlreadyImports(t *testing.T) {
	_, generate := wireModule(t, map[string]string{"user": "graph/User.Handler", "post": "graph/post.Handler"})
	if err := generate(); err == nil || !strings.Contains(err.Error(), "letter case") {
		t.Fatalf("err = %v, want graph/User refused beside the group package graph/user", err)
	}
}

// Where the directory exists, its name on disk is the spelling: the first one
// met in the configuration may be the wrong one, and every other file that
// imports the package uses the directory's.
func TestWireSpellsADirectoryAsItIsOnDisk(t *testing.T) {
	scaffold := map[string]string{"post": "internal/Resolver.R", "user": "internal/resolver.R"}
	dir, generate := wireModule(t, scaffold)
	if err := os.MkdirAll(filepath.Join(dir, "internal", "resolver"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !caseInsensitive(t, dir) {
		t.Skip("case-sensitive filesystem: the two spellings are two directories, refused above")
	}
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
	if !strings.Contains(wire, `"hello/internal/resolver"`) || strings.Contains(wire, `"hello/internal/Resolver"`) {
		t.Errorf("the directory is not imported under its name on disk:\n%s", wire)
	}
	if !testing.Short() {
		goBuild(t, dir)
	}
}

// A test file is read as the other files are: one no build includes is not
// there, and a constructor is a name however it is declared.
func TestWireReadsTestFilesAsItReadsTheRest(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct {
		file     string
		scaffold bool
	}{
		"a constructor declared as a variable": {"package user\n\nimport \"hello/internal/deps\"\n\nvar NewHandler = func(d deps.Deps) *Handler { return &Handler{} }\n", false},
		"a test file no build includes":        {"//go:build ignore\n\npackage user\n\nimport \"hello/internal/deps\"\n\nfunc NewHandler(d deps.Deps) *Handler { return &Handler{} }\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wireModule(t, scaffold)
			writeFiles(t, dir, map[string]string{"graph/user/handler_test.go": c.file})
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			stubs := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
			if got := strings.Contains(stubs, "func NewHandler"); got != c.scaffold {
				t.Errorf("constructor scaffolded = %v, want %v:\n%s", got, c.scaffold, stubs)
			}
		})
	}
}

// wirePassModule is wireFieldModule with Wire.Pass set.
func wirePassModule(t *testing.T, scaffold map[string]string, deps string, pass map[string]string) (string, func() error) {
	t.Helper()
	dir, _ := wireFieldModule(t, scaffold, deps)
	return dir, func() error {
		return Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph",
			Scaffold: scaffold,
			Wire:     Wire{Deps: "hello/internal/deps.Deps", Pass: pass},
		})
	}
}

// What a constructor is given is read from the dependencies struct and from
// the constructor, without type information, and that reading has a long
// tail. Wrong, it lands in wire.go, which nobody may edit, and the only way
// out was to rearrange one's own code around the generator. Wire.Pass says it
// outright, as the expression wire.go writes, and is believed.
func TestWirePassSaysWhatAGroupIsGiven(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	const accounts = "package deps\n\nimport (\n\t\"hello/internal/svc/post\"\n\tusersvc \"hello/internal/svc/user\"\n)\n\n// Deps is the application's shared dependencies.\ntype Deps struct {\n\tAccounts *usersvc.Service\n\tUser     *usersvc.Service\n\tPost     post.Service\n}\n"
	for name, c := range map[string]struct {
		deps  string
		pass  map[string]string
		files map[string]string
		wire  string
		ctor  string // in graph/user/user.resolvers.go, when it is scaffolded
	}{
		// The field rule would hand over d.User.
		"the whole value, for a group that has a field": {
			deps: wireFieldDeps, pass: map[string]string{"user": "d"},
			wire: "user.Bindings(user.NewHandler(d))", ctor: "func NewHandler(d deps.Deps) *Handler",
		},
		"a pointer to the whole value": {
			deps: wireFieldDeps, pass: map[string]string{"user": "&d"},
			wire: "user.Bindings(user.NewHandler(&d))", ctor: "func NewHandler(d *deps.Deps) *Handler",
		},
		// The group is user and the field is not.
		"a field under another name": {
			deps: accounts, pass: map[string]string{"user": "d.Accounts"},
			wire: "user.Bindings(user.NewHandler(d.Accounts))", ctor: "func NewHandler(dep *usersvc.Service) *Handler",
		},
		"a pointer to a field": {
			deps: wireFieldDeps, pass: map[string]string{"post": "&d.Post", "user": "d.User"},
			wire: "post.Bindings(post.NewHandler(&d.Post))",
		},
		// A constructor the reading gets wrong: it takes an interface the
		// whole value satisfies, there is a field named after the group, and
		// nothing in the source says which was meant.
		"over a constructor that is misread": {
			deps: wireFieldDeps, pass: map[string]string{"user": "d"},
			files: map[string]string{
				"graph/user/handler.go": "package user\n\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler takes anything.\nfunc NewHandler(any) *Handler { return &Handler{} }\n",
			},
			wire: "user.Bindings(user.NewHandler(d))",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wirePassModule(t, both, c.deps, c.pass)
			writeFiles(t, dir, c.files)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm(c.wire)) {
				t.Errorf("wire.go lacks %q\n%s", c.wire, wire)
			}
			if c.ctor != "" {
				got := readFile(t, filepath.Join(dir, "graph", "user", "user.resolvers.go"))
				if !strings.Contains(norm(got), norm(c.ctor)) {
					t.Errorf("the scaffolded constructor is not %q\n%s", c.ctor, got)
				}
				// Its comment says what it is built from, pointer or not.
				if pointer := strings.HasPrefix(c.pass["user"], "&"); pointer != strings.Contains(got, "from a pointer to") {
					t.Errorf("the constructor's comment says pointer=%v for the entry %q\n%s", !pointer, c.pass["user"], got)
				}
			}
			// The same on a second run: what was scaffolded is read back.
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm(c.wire)) {
				t.Errorf("a second run changed wire.go; it lacks %q\n%s", c.wire, wire)
			}
			goBuild(t, dir)
		})
	}
}

// An entry is believed, so one that cannot be right is refused where it is
// written rather than left for the compiler to report in generated code.
func TestWirePassRefusesWhatCannotBeMeant(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct {
		scaffold map[string]string
		deps     string
		pass     map[string]string
		want     string
	}{
		"a group that is not one":             {both, wireFieldDeps, map[string]string{"usr": "d"}, "usr"},
		"something that is not an expression": {both, wireFieldDeps, map[string]string{"user": "the user service"}, "d, &d, d.Field"},
		"a path through a field":              {both, wireFieldDeps, map[string]string{"user": "d.User.Greeting"}, "d, &d, d.Field"},
		"a field the struct does not have":    {both, wireFieldDeps, map[string]string{"user": "d.Users"}, "User"},
		"a field another package cannot read": {both, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tuser *usersvc.Service", 1), map[string]string{"user": "d.user"}, "exported"},
		// One type is built once, from one thing.
		"two things for one shared type": {
			map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}, wireFieldDeps,
			map[string]string{"user": "d.User", "post": "d.Post"}, "built once",
		},
		// Scaffolding has to write the field's type, and this one it cannot.
		"a field whose type cannot be written, for a constructor still to be written": {
			both, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tUser struct{ n int }", 1), map[string]string{"user": "d.User"}, "write the constructor",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wirePassModule(t, c.scaffold, c.deps, c.pass)
			err := generate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "wire") {
				t.Errorf("the error does not say where the entry is: %v", err)
			}
			if _, serr := os.Stat(filepath.Join(dir, "graph", "wire.go")); serr == nil {
				t.Error("wire.go was written although the entry was refused")
			}
		})
	}
}

// With no Wire there is nothing for an entry to apply to, and it is more
// likely that deps was left out than that pass was meant to do nothing.
func TestWirePassNeedsWire(t *testing.T) {
	dir, _ := scaffoldModule(t, map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"})
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Wire: Wire{Pass: map[string]string{"user": "d"}},
	})
	if err == nil || !strings.Contains(err.Error(), "Wire.Deps") {
		t.Fatalf("err = %v, want Pass without Deps refused", err)
	}
}

// A group that is not listed is decided as it was.
func TestWirePassLeavesOtherGroupsAlone(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, generate := wirePassModule(t, both, wireFieldDeps, map[string]string{"user": "d"})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm("post.Bindings(post.NewHandler(d.Post))")) {
		t.Errorf("the group not listed lost its field:\n%s", wire)
	}
}

// A review of Wire.Pass, which ran the generator. An entry has to be able to
// say whatever the right argument is, or the way out it offers is closed
// exactly where it is needed.
func TestWirePassReachesWhatTheStructDoesNotSpellOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	handler := func(pkg, imports, param string) string {
		return "package " + pkg + "\n\n" + imports + "\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler is hand-written.\nfunc NewHandler(" + param + ") *Handler { return &Handler{} }\n"
	}
	const embedded = "package deps\n\nimport (\n\t\"hello/internal/svc/post\"\n\tusersvc \"hello/internal/svc/user\"\n)\n\n// Core is embedded.\ntype Core struct{ Accounts *usersvc.Service }\n\n// Deps is the dependencies.\ntype Deps struct {\n\tCore\n\t*post.Service\n}\n"
	userTakesService := handler("user", "import usersvc \"hello/internal/svc/user\"\n", "s *usersvc.Service")
	postTakesService := handler("post", "import postsvc \"hello/internal/svc/post\"\n", "s *postsvc.Service")
	for name, c := range map[string]struct {
		deps  string
		pass  map[string]string
		files map[string]string
		want  []string
	}{
		// A promoted field, and an embedded one, are fields: d.Accounts and
		// d.Service compile, and only the entry can say so.
		"a field promoted from an embedded struct, and an embedded field": {
			deps: embedded, pass: map[string]string{"user": "d.Accounts", "post": "d.Service"},
			files: map[string]string{"graph/user/handler.go": userTakesService, "graph/post/handler.go": postTakesService},
			want:  []string{"user.NewHandler(d.Accounts)", "post.NewHandler(d.Service)"},
		},
		// An embedded field has a name and a type like any other, so a
		// constructor can be written over it.
		"an embedded field, for a constructor still to be written": {
			deps: embedded, pass: map[string]string{"user": "d", "post": "d.Service"},
			want: []string{"post.NewHandler(d.Service)"},
		},
		"an embedded generic type, for a constructor still to be written": {
			deps: "package deps\n\n// Box is generic.\ntype Box[T any] struct{ V T }\n\n// Deps is the dependencies.\ntype Deps struct {\n\tBox[int]\n\tPost int\n}\n",
			pass: map[string]string{"user": "d.Box"},
			want: []string{"user.NewHandler(d.Box)"},
		},
		// The struct cannot be read -- two files declare it -- so the entry
		// cannot be checked, and with constructors that exist it need not be.
		"a field of a struct gqlc cannot read": {
			deps: "//go:build !linux\n\n" + wireFieldDeps, pass: map[string]string{"user": "d.User", "post": "d.Post"},
			files: map[string]string{
				"internal/deps/deps_linux.go": wireFieldDeps,
				"graph/user/handler.go":       userTakesService,
				"graph/post/handler.go":       handler("post", "import postsvc \"hello/internal/svc/post\"\n", "s postsvc.Service"),
			},
			want: []string{"user.NewHandler(d.User)", "post.NewHandler(d.Post)"},
		},
		// One of two fields that are one name once folded: named exactly, it
		// is no longer ambiguous, and its constructor can be written.
		"one of two fields that fold alike": {
			deps: strings.Replace(wireFieldDeps, "\tUser *usersvc.Service\n", "\tUser *usersvc.Service\n\tU_ser int\n", 1),
			pass: map[string]string{"user": "d.User"},
			want: []string{"user.NewHandler(d.User)", "post.NewHandler(d.Post)"},
		},
		"an entry with spaces round it": {
			deps: wireFieldDeps, pass: map[string]string{"user": "  d  "},
			want: []string{"user.NewHandler(d)"},
		},
		// A constructor is a name in the package however it is declared, and
		// one that is there is not asked to be written.
		"a constructor assigned from another function": {
			deps: "package deps\n\nimport \"hello/internal/svc/post\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser interface{ Hello() string }\n\tPost post.Service\n}\n",
			pass: map[string]string{"user": "d.User"},
			files: map[string]string{
				"graph/user/handler.go": "package user\n\n// Handler is hand-written.\ntype Handler struct{}\n\nfunc build(interface{ Hello() string }) *Handler { return &Handler{} }\n\n// NewHandler is assigned.\nvar NewHandler = build\n",
			},
			want: []string{"user.NewHandler(d.User)"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wirePassModule(t, both, c.deps, c.pass)
			writeFiles(t, dir, c.files)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
			for _, want := range c.want {
				if !strings.Contains(norm(wire), norm(want)) {
					t.Errorf("wire.go lacks %q\n%s", want, wire)
				}
			}
			// The constructor's comment names the field it is built from.
			if strings.HasPrefix(name, "an embedded field,") {
				if got := readFile(t, filepath.Join(dir, "graph", "post", "post.resolvers.go")); !strings.Contains(got, "the Service field") {
					t.Errorf("the scaffolded constructor's comment does not name the embedded field:\n%s", got)
				}
			}
			goBuild(t, dir)
		})
	}
}

// One type implementing several groups is one constructor and one value, so
// one entry speaks for all of them: the group it is written under is which
// line of the configuration it happens to be on.
func TestWirePassForASharedTypeIsSaidOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	shared := map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}
	for _, listed := range []string{"user", "post"} {
		t.Run("under "+listed, func(t *testing.T) {
			dir, generate := wirePassModule(t, shared, wireFieldDeps, map[string]string{listed: "d.User"})
			if err := generate(); err != nil {
				t.Fatalf("one entry for a shared type was refused: %v", err)
			}
			wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
			if !strings.Contains(norm(wire), norm("r := resolver.NewR(d.User)")) {
				t.Errorf("the shared type is not built once from the entry:\n%s", wire)
			}
			impl := readFile(t, filepath.Join(dir, "internal", "resolver", "post.resolvers.go"))
			if !strings.Contains(norm(impl), norm("func NewR(dep *usersvc.Service) *R")) {
				t.Errorf("the scaffolded constructor does not take what the entry says:\n%s", impl)
			}
			goBuild(t, dir)
		})
	}
	// Two entries that disagree are refused naming both.
	_, generate := wirePassModule(t, shared, wireFieldDeps, map[string]string{"user": "d.User", "post": "d.Post"})
	err := generate()
	if err == nil || !strings.Contains(err.Error(), "user") || !strings.Contains(err.Error(), "post") || !strings.Contains(err.Error(), "d.User") || !strings.Contains(err.Error(), "d.Post") {
		t.Fatalf("err = %v, want both groups and both entries named", err)
	}
}

// What the first tests of Wire.Pass left unguarded, each found by breaking it
// and seeing nothing fail.
func TestWirePassEdges(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	run := func(t *testing.T, scaffold map[string]string, deps string, pass map[string]string, files map[string]string) (string, []string, error) {
		t.Helper()
		dir, _ := wireFieldModule(t, scaffold, deps)
		writeFiles(t, dir, files)
		var notes []string
		err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
			Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps", Pass: pass},
			Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
		})
		return dir, notes, err
	}
	twoFiles := map[string]string{"internal/deps/deps_linux.go": wireFieldDeps}

	t.Run("a field of an unreadable struct, for a constructor still to be written, is refused", func(t *testing.T) {
		dir, _, err := run(t, both, "//go:build !linux\n\n"+wireFieldDeps, map[string]string{"user": "d.User"}, twoFiles)
		if err == nil || !strings.Contains(err.Error(), "cannot read") {
			t.Fatalf("err = %v, want the entry refused because the field's type is unknown", err)
		}
		if _, serr := os.Stat(filepath.Join(dir, "graph", "wire.go")); serr == nil {
			t.Error("wire.go was written")
		}
	})
	t.Run("a blank field is not a field", func(t *testing.T) {
		if _, _, err := run(t, both, wireFieldDeps, map[string]string{"user": "d._"}, nil); err == nil || !strings.Contains(err.Error(), "d, &d, d.Field") {
			t.Fatalf("err = %v, want d._ refused", err)
		}
	})
	t.Run("an empty entry says what YAML did to it", func(t *testing.T) {
		if _, _, err := run(t, both, wireFieldDeps, map[string]string{"user": ""}, nil); err == nil || !strings.Contains(err.Error(), "quote") {
			t.Fatalf("err = %v, want the empty entry explained: an unquoted &d is a YAML anchor", err)
		}
	})
	t.Run("the fields offered are the ones the entry could name, in order", func(t *testing.T) {
		deps := strings.Replace(wireFieldDeps, "type Deps struct {", "type Deps struct {\n\tsecret int\n\tZebra int\n\tAlpha int", 1)
		_, _, err := run(t, both, deps, map[string]string{"user": "d.Nope"}, nil)
		if err == nil || !strings.Contains(err.Error(), "Alpha, Post, User, Zebra") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("err = %v, want the exported fields, sorted, and no other", err)
		}
	})
	t.Run("an entry for no group names the groups there are, in order", func(t *testing.T) {
		if _, _, err := run(t, both, wireFieldDeps, map[string]string{"zzz": "d", "aaa": "d"}, nil); err == nil || !strings.Contains(err.Error(), "aaa") || !strings.Contains(err.Error(), "post, user") {
			t.Fatalf("err = %v, want the first unknown key in order and the groups listed", err)
		}
	})
	t.Run("a listed group is not told it has no field", func(t *testing.T) {
		_, notes, err := run(t, both, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service\n", "", 1), map[string]string{"user": "d"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if said := strings.Join(notes, "\n"); strings.Contains(said, "no field named after it") {
			t.Errorf("the whole value was asked for, and the note advises against it:\n%s", said)
		}
	})
	// Others being handed a field is what makes one that is not worth a word,
	// and a group whose entry asks for the whole value is not being handed one.
	t.Run("a group given the whole value by its entry is not counted as having a field", func(t *testing.T) {
		deps := strings.Replace(wireFieldDeps, "\tPost post.Service\n", "\tItems post.Service\n", 1)
		_, notes, err := run(t, both, deps, map[string]string{"user": "d"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if said := strings.Join(notes, "\n"); strings.Contains(said, "no field named after it") {
			t.Errorf("no group is handed a field, and post was told others are:\n%s", said)
		}
	})
}

// With one group everything is in the generated package, and so may the
// dependencies type be. Its unexported fields are then in reach of wire.go.
func TestWirePassMayNameAnUnexportedFieldInItsOwnPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema/schema.graphql": "type Thing { id: ID! }\ntype Query { thing(id: ID!): Thing }\n",
		"graph/deps.go":         "package graph\n\n// Deps is the dependencies.\ntype Deps struct{ store *Store }\n\n// Store is a store.\ntype Store struct{}\n",
	})
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"graph": "graph.Handler"},
		Wire:     Wire{Deps: "hello/graph.Deps", Pass: map[string]string{"graph": "d.store"}},
	}); err != nil {
		t.Fatal(err)
	}
	if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm("Bindings(NewHandler(d.store))")) {
		t.Errorf("wire.go does not pass the unexported field:\n%s", wire)
	}
	goBuild(t, dir)
}

// A sixth review, with mutation testing. A group with no entry is decided as
// it was before Wire.Pass existed, and recording a constructor whose
// parameter cannot be read must not change that: it is there, so nothing is
// scaffolded over it, and what it takes is unknown, so nothing is inferred
// from it.
func TestWireInfersNothingFromAConstructorItCannotRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	const unexported = "package deps\n\ntype secret struct{}\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser secret\n\tPost int\n}\n"
	const assigned = "package user\n\nimport \"hello/internal/deps\"\n\n// Handler is hand-written.\ntype Handler struct{}\n\nfunc build(deps.Deps) *Handler { return &Handler{} }\n\n// NewHandler is assigned.\nvar NewHandler = build\n"
	for name, c := range map[string]struct {
		deps  string
		files map[string]string
	}{
		// The field's type cannot be written elsewhere, so before the
		// constructor was recorded this group was given d. It still is.
		"assigned from a function, beside a field that cannot be handed over": {
			deps: unexported, files: map[string]string{"graph/user/handler.go": assigned},
		},
		// Two files a build chooses between: the one that says what it
		// takes is the one to read, whichever comes first by name.
		"an unreadable one in the first file and a function in the second": {
			deps: wireFieldDeps,
			files: map[string]string{
				"graph/user/a_linux.go": "//go:build linux\n\n" + assigned,
				"graph/user/b_other.go": "//go:build !linux\n\npackage user\n\nimport \"hello/internal/deps\"\n\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler is hand-written.\nfunc NewHandler(deps.Deps) *Handler { return &Handler{} }\n",
			},
		},
		"a function in the first file and an unreadable one in the second": {
			deps: wireFieldDeps,
			files: map[string]string{
				"graph/user/a_other.go": "//go:build !linux\n\npackage user\n\nimport \"hello/internal/deps\"\n\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler is hand-written.\nfunc NewHandler(deps.Deps) *Handler { return &Handler{} }\n",
				"graph/user/b_linux.go": "//go:build linux\n\n" + assigned,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, generate := wireFieldModule(t, both, c.deps)
			writeFiles(t, dir, c.files)
			if err := generate(); err != nil {
				t.Fatal(err)
			}
			if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm("user.Bindings(user.NewHandler(d))")) {
				t.Errorf("the group is not given the whole value:\n%s", wire)
			}
			goBuild(t, dir)
		})
	}
}

// What the entry's errors say, exactly: which group the entry is under, not
// which group happened to be looked at when it was found, and in an order
// that does not change.
func TestWirePassErrorsNameTheGroupTheEntryIsUnder(t *testing.T) {
	// post sorts first and has no entry; the entry is user's.
	shared := map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}
	inline := strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tUser struct{ n int }", 1)
	twoFiles := map[string]string{"internal/deps/deps_linux.go": wireFieldDeps}
	for name, c := range map[string]struct {
		scaffold map[string]string
		deps     string
		pass     map[string]string
		files    map[string]string
		want     string
	}{
		"an empty entry":             {shared, wireFieldDeps, map[string]string{"user": ""}, nil, "wire.pass: user: the entry is empty"},
		"not an expression":          {shared, wireFieldDeps, map[string]string{"user": "the service"}, nil, `wire.pass: user: "the service"`},
		"a field that is not there":  {shared, wireFieldDeps, map[string]string{"user": "d.Nope"}, nil, "wire.pass: user: Deps has no field Nope"},
		"a field that is unexported": {shared, strings.Replace(wireFieldDeps, "\tUser *usersvc.Service", "\tuser *usersvc.Service", 1), map[string]string{"user": "d.user"}, nil, "wire.pass: user: Deps.user is not exported"},
		"a type that cannot be written, for a constructor to write": {shared, inline, map[string]string{"user": "d.User"}, nil, "wire.pass: user: internal/resolver.R has no NewR yet"},
		"a struct that cannot be read, for a constructor to write": {
			shared, "//go:build !linux\n\n" + wireFieldDeps, map[string]string{"user": "d.User"}, twoFiles, "wire.pass: user: internal/resolver.R has no NewR yet, and gqlc cannot read",
		},
		// Three groups, one without an entry and two that differ: named in
		// name order, each with its own entry.
		"two entries that differ": {
			map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}, wireFieldDeps,
			map[string]string{"user": "d.User", "post": "d.Post"}, nil, "wire.pass: post and user are implemented by one type, which is built once from one thing, and are given d.Post and d.User",
		},
		// An unexported field is out of wire.go's reach whether or not the
		// struct could be read, and the name is all that takes to know.
		"an unexported field of a struct that cannot be read": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}, "//go:build !linux\n\n" + wireFieldDeps,
			map[string]string{"user": "d.user"}, twoFiles, "wire.pass: user: Deps.user is not exported",
		},
		"an unexported field that might be promoted": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"},
			"package deps\n\n// Core is embedded.\ntype Core struct{ accounts int }\n\n// Deps is the dependencies.\ntype Deps struct {\n\tCore\n\tPost int\n}\n",
			map[string]string{"user": "d.accounts"}, nil, "wire.pass: user: Deps.accounts is not exported",
		},
		// The struct was read, and what it embeds was not: that is what to
		// say, not that the struct could not be read.
		"a field that is not declared, of a struct that embeds, for a constructor to write": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"},
			"package deps\n\n// Core is embedded.\ntype Core struct{ Accounts int }\n\n// Deps is the dependencies.\ntype Deps struct {\n\tCore\n\tPost int\n}\n",
			map[string]string{"user": "d.Accounts"}, nil, "embeds",
		},
		// Unexported, and the implementation is not in the dependencies
		// package: the check is made for where the constructor will be.
		"an unexported type, for a constructor to write elsewhere": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"},
			"package deps\n\ntype store struct{}\n\n// Deps is the dependencies.\ntype Deps struct {\n\tUser *store\n\tPost int\n}\n",
			map[string]string{"user": "d.User"}, nil, "wire.pass: user: graph/user.Handler has no NewHandler yet, and gqlc cannot write one that takes d.User (its type names store, which no other package can)",
		},
		// A bare embedded type in a file with a dot-import may be either
		// package's.
		"an embedded type that a dot-import leaves ambiguous, for a constructor to write": {
			map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"},
			"package deps\n\nimport . \"hello/internal/svc/user\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tService\n\tPost int\n}\n",
			map[string]string{"user": "d.Service"}, nil, "dot-import",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := wireFieldModule(t, c.scaffold, c.deps)
			writeFiles(t, dir, c.files)
			err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
				Scaffold: c.scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps", Pass: c.pass},
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v\nwant one containing %q", err, c.want)
			}
			// Refused before anything is written, not by a later stage that
			// finds the same fault with wire.go already on disk.
			if _, serr := os.Stat(filepath.Join(dir, "graph", "wire.go")); serr == nil {
				t.Error("wire.go was written although the entry was refused")
			}
		})
	}
}

// Three groups on one type: the first by name has no entry and the other two
// disagree. The two named are the two that disagree, not the one being
// decided when the disagreement was met.
func TestWirePassNamesTheTwoGroupsThatDisagree(t *testing.T) {
	scaffold := map[string]string{"post": "internal/resolver.R", "tag": "internal/resolver.R", "user": "internal/resolver.R"}
	dir, _ := wireFieldModule(t, scaffold, wireFieldDeps)
	writeFiles(t, dir, map[string]string{"schema/tag.graphql": "type Tag { id: ID! }\nextend type Query { tags: [Tag!]! }\n"})
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps", Pass: map[string]string{"tag": "d.Post", "user": "d.User"}},
	})
	const want = "wire.pass: tag and user are implemented by one type, which is built once from one thing, and are given d.Post and d.User"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v\nwant one containing %q", err, want)
	}
}

// What one implementation is, for an entry to speak for all of it and for
// nothing else.
func TestWirePassKeepsToItsOwnImplementation(t *testing.T) {
	// Two types in one directory are two implementations.
	t.Run("another type in the same directory", func(t *testing.T) {
		dir, generate := wirePassModule(t, map[string]string{"user": "internal/resolver.A", "post": "internal/resolver.B"}, wireFieldDeps, map[string]string{"user": "d"})
		if err := generate(); err != nil {
			t.Fatal(err)
		}
		wire := readFile(t, filepath.Join(dir, "graph", "wire.go"))
		for _, want := range []string{"user.Bindings(resolver.NewA(d))", "post.Bindings(resolver.NewB(d.Post))"} {
			if !strings.Contains(norm(wire), norm(want)) {
				t.Errorf("wire.go lacks %q\n%s", want, wire)
			}
		}
	})
	// Entries that differ only in the spaces round them say one thing.
	t.Run("the same entry, spaced differently", func(t *testing.T) {
		_, generate := wirePassModule(t, map[string]string{"user": "internal/resolver.R", "post": "internal/resolver.R"}, wireFieldDeps, map[string]string{"user": "d", "post": " d "})
		if err := generate(); err != nil {
			t.Fatalf("refused, though both entries say d: %v", err)
		}
	})
	// One directory under two spellings is one implementation, where the
	// filesystem says so.
	t.Run("a directory spelled two ways", func(t *testing.T) {
		scaffold := map[string]string{"user": "internal/Resolver.R", "post": "internal/resolver.R"}
		dir, generate := wirePassModule(t, scaffold, wireFieldDeps, map[string]string{"user": "d.User"})
		if err := os.MkdirAll(filepath.Join(dir, "internal", "resolver"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !caseInsensitive(t, dir) {
			t.Skip("case-sensitive filesystem: the two spellings are two directories, refused elsewhere")
		}
		if err := generate(); err != nil {
			t.Fatal(err)
		}
		if wire := readFile(t, filepath.Join(dir, "graph", "wire.go")); !strings.Contains(norm(wire), norm("r := resolver.NewR(d.User)")) {
			t.Errorf("the entry under one spelling did not speak for the other:\n%s", wire)
		}
	})
}

// The note that a new group has no field while others have theirs: an entry
// naming a field is a group being handed one, and a struct that could not be
// read says nothing about what fields there are.
func TestWirePassAndTheNoteAboutFields(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	note := func(t *testing.T, deps string, pass map[string]string, files map[string]string) string {
		t.Helper()
		dir, _ := wireFieldModule(t, both, deps)
		writeFiles(t, dir, files)
		var notes []string
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
			Scaffold: both, Wire: Wire{Deps: "hello/internal/deps.Deps", Pass: pass},
			Notef: func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(notes, "\n")
	}
	const accounts = "package deps\n\nimport usersvc \"hello/internal/svc/user\"\n\n// Deps is the dependencies.\ntype Deps struct {\n\tAccounts *usersvc.Service\n}\n"
	if said := note(t, accounts, map[string]string{"user": "d.Accounts"}, nil); !strings.Contains(said, "the post group has no field named after it") {
		t.Errorf("user is handed a field by its entry, and post, which has none, was not told:\n%s", said)
	}
	userTakesService := "package user\n\nimport usersvc \"hello/internal/svc/user\"\n\n// Handler is hand-written.\ntype Handler struct{}\n\n// NewHandler is hand-written.\nfunc NewHandler(*usersvc.Service) *Handler { return &Handler{} }\n"
	if said := note(t, "//go:build !linux\n\n"+wireFieldDeps, map[string]string{"user": "d.User"},
		map[string]string{"internal/deps/deps_linux.go": wireFieldDeps, "graph/user/handler.go": userTakesService}); strings.Contains(said, "no field named after it") {
		t.Errorf("the struct could not be read, and post was told it has no field in it:\n%s", said)
	}
}

// With the dependencies in the generated package every field is in reach,
// and the blank one is still not a field.
func TestWirePassOffersNoBlankField(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema/schema.graphql": "type Thing { id: ID! }\ntype Query { thing(id: ID!): Thing }\n",
		"graph/deps.go":         "package graph\n\n// Deps is the dependencies.\ntype Deps struct {\n\t_     int\n\tstore int\n\tOpen  int\n}\n",
	})
	writeTempModule(t, dir)
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"graph": "graph.Handler"},
		Wire:     Wire{Deps: "hello/graph.Deps", Pass: map[string]string{"graph": "d.Nope"}},
	})
	if err == nil || !strings.Contains(err.Error(), "it has Open, store") {
		t.Fatalf("err = %v, want the two fields that can be named and not the blank one", err)
	}
}
