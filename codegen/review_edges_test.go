package codegen

import (
	"context"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A group is a Go package named after an SDL file, so func.graphql asked for
// `package func`, main.graphql for a package nothing can import, and
// graphql.graphql, context.graphql and embed.graphql for a package whose name
// the generated code already uses for an import.
func TestAGroupIsNeverNamedSomethingGoOrTheGeneratorReserves(t *testing.T) {
	for _, stem := range []string{
		"func", "type", "go", "range", "select", "main", "init", "graphql", "context", "embed",
		// Predeclared: the generated code spells its types and calls with them.
		"error", "string", "int", "bool", "any", "append", "nil", "true", "float64",
		// Declared by the generated code where a group's packages are in scope.
		"r", "v", "a", "ctx", "all", "err",
	} {
		got := sanitizeGroup(stem, "graph", false)
		if got == stem || token.IsKeyword(got) {
			t.Errorf("sanitizeGroup(%q) = %q, which cannot name a generated package", stem, got)
		}
	}
	// Ordinary names are left alone.
	if got := sanitizeGroup("user", "graph", false); got != "user" {
		t.Errorf("sanitizeGroup(%q) = %q", "user", got)
	}
}

// An enum's constants are named Type+Value, which is also a legal type name:
// enum Color { RED } beside type ColorRed generated two declarations of
// ColorRed, found by the compiler in a file the author is told not to edit.
func TestAnEnumConstantThatCollidesWithATypeIsRefused(t *testing.T) {
	_, err := generateWith(t, `
		enum Color { RED GREEN }
		type ColorRed { id: ID! }
		input ColorGreen { id: ID! }
		type Query { c: Color r: ColorRed g(in: ColorGreen): Int }
	`, Config{})
	if err == nil {
		t.Fatal("generated two Go declarations of one name")
	}
	for _, want := range []string{"Color.RED", "ColorRed", "Color.GREEN", "ColorGreen"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

const jsonAndGoTagSDL = `
directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION
input CreateThing {
  name: String @goTag(key: "json", value: "custom_name")
  other: String
}
type Query { thing(input: CreateThing): String }
`

// JSONTags and a @goTag for json each wrote a json key, and reflect answers
// with the first: the tag the author spelled out was silently dead.
func TestATagDirectiveForJSONReplacesTheGeneratedJSONTag(t *testing.T) {
	dir := generate(t, jsonAndGoTagSDL, Config{TagDirective: goTag, JSONTags: true})
	models := norm(generated(t, dir, "model/models.go"))
	if !strings.Contains(models, norm("`graphql:\"name\" json:\"custom_name\"`")) {
		t.Errorf("the directive's json tag is not the field's only one:\n%s", models)
	}
	if !strings.Contains(models, norm("`graphql:\"other\" json:\"other,omitempty\"`")) {
		t.Errorf("a field without the directive lost its generated json tag:\n%s", models)
	}
}

func TestTagDirectiveRefusesWhatAStructTagCannotCarry(t *testing.T) {
	for name, directive := range map[string]string{
		"control character in the key":   `@goTag(key: "v\u0001", value: "x")`,
		"control character in the value": `@goTag(key: "v", value: "a\u0000b")`,
		"a null value":                   `@goTag(key: "v", value: null)`,
	} {
		t.Run(name, func(t *testing.T) {
			sdl := `
				directive @goTag(key: String!, value: String) repeatable on INPUT_FIELD_DEFINITION
				input In { name: String ` + directive + ` }
				type Query { f(in: In): Int }`
			_, err := generateWith(t, sdl, Config{TagDirective: goTag})
			if err == nil {
				t.Fatal("generated a struct tag from it")
			}
			if !strings.Contains(err.Error(), "In.name") || strings.Contains(err.Error(), "package model") {
				t.Errorf("want an error naming In.name, not a dump of the generated source: %v", err)
			}
		})
	}
}

// A FieldNames key that names nothing in the schema is a typo, and was
// accepted in silence where every other configuration key is checked.
func TestFieldNamesRefusesANameTheSchemaDoesNotHave(t *testing.T) {
	sdl := `type T { userId: ID! } type Query { t(byId: ID): T }`
	if _, err := generateWith(t, sdl, Config{FieldNames: map[string]string{"userId": "UserID", "byId": "ByID"}}); err != nil {
		t.Fatalf("a field name and an argument name were refused: %v", err)
	}
	first := ""
	for range 20 {
		_, err := generateWith(t, sdl, Config{FieldNames: map[string]string{"usrId": "UserID", "zzz": "Z", "aaa": "A"}})
		if err == nil {
			t.Fatal("FieldNames keys that name no field or argument were accepted")
		}
		for _, want := range []string{"usrId", "zzz", "aaa"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error does not name %s: %v", want, err)
			}
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("the same configuration gave two errors:\n%s\n%s", first, err)
		}
	}
}

// The engine binds a scalar or enum through two list levels and an input
// object through one. SDL nesting deeper than that generated Go that compiled
// and a schema that could not be built, with nothing at generate time to say
// so -- and no Go type the author could substitute, since the limit is on the
// position, not on the model.
func TestListsNestedDeeperThanTheEngineBindsAreRefused(t *testing.T) {
	_, err := generateWith(t, `
		input Point { x: Float! y: Float! }
		input Poly { rings: [[Point!]!]  ok: [Point!] }
		type Geo { coordinates: [[[Float!]!]!]!  ok: [[Float!]!]! }
		type Query { geo: Geo  grid(cells: [[[Int]]], poly: Poly): Int }
	`, Config{})
	if err == nil {
		t.Fatal("generated bindings NewSchema cannot build")
	}
	for _, want := range []string{"Poly.rings", "Geo.coordinates", "Query.grid(cells:)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	for _, fine := range []string{"Poly.ok", "Geo.ok"} {
		if strings.Contains(err.Error(), fine) {
			t.Errorf("%s is within what the engine binds and was refused: %v", fine, err)
		}
	}
}

// What the engine does bind still generates, including an object list of any
// depth, which goes through the reflection traverser.
func TestListsTheEngineBindsStillGenerate(t *testing.T) {
	if _, err := generateWith(t, `
		input Point { x: Float! }
		type Cell { id: ID! }
		type Query { a(p: [Point!], m: [[Int!]!]): [[Float!]!]  grid: [[[Cell!]!]!] }
	`, Config{}); err != nil {
		t.Fatalf("refused lists the engine binds: %v", err)
	}
}

// A package path whose last element is not an identifier, or a model that is
// not a Go type expression, was written into the output as it stood, and the
// error was gofmt's: a line and column in a file that was never written,
// followed by the whole of its source.
func TestConfigThatCannotBeGoIsRefusedByName(t *testing.T) {
	sdl := `type User { id: ID! } type Query { me: User }`
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"package is not an identifier": {Config{}, "Package"},
		"package is a keyword":         {Config{}, "Package"},
		"model is not a type":          {Config{Models: map[string]string{"User": "example.com/app.not a type!"}}, "Models"},
		"model has no type name":       {Config{Models: map[string]string{"User": "example.com/app."}}, "Models"},
		"model is a path alone":        {Config{Models: map[string]string{"User": "example.com/app"}}, `Models["User"]`},
		"model is a map over a path":   {Config{Models: map[string]string{"User": "map[string]encoding/json.RawMessage"}}, `Models["User"]`},
		"model is a generic over one":  {Config{Models: map[string]string{"User": "example.com/x.Box[example.com/y.T]"}}, `Models["User"]`},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"schema.graphql": sdl})
			cfg := tc.cfg
			cfg.Dir, cfg.SchemaGlobs, cfg.Output, cfg.Package = dir, []string{"schema.graphql"}, "graph", "hello/graph"
			switch name {
			case "package is not an identifier":
				cfg.Package = "hello/1-graph"
			case "package is a keyword":
				cfg.Package = "hello/func"
			}
			err := Generate(context.Background(), cfg)
			if err == nil {
				t.Fatal("generated from configuration that cannot be Go")
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "DO NOT EDIT") {
				t.Errorf("want an error about %s, not a dump of generated source: %v", tc.want, err)
			}
		})
	}
}

// A Models entry is split into its package and its type at the last dot. At
// the first one after the last slash, gopkg.in/guregu/null.v4.String was the
// package gopkg.in/guregu/null and the type v4.String: a formatter error over
// a file that was never written, for a path the go command accepts.
func TestAModelPathMayHaveADotInItsLastElement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema.graphql":  "scalar Stamp\ninput Range { from: Stamp to: [Stamp!] }\ntype Event { at: Stamp! }\ntype Query { events(in: Range): [Event!]! }\n",
		"null.v4/null.go": "package null\n\ntype Time struct{ Valid bool }\n",
	})
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
		Models: map[string]string{"Stamp": "hello/null.v4.Time"},
	}); err != nil {
		t.Fatal(err)
	}
	if src := readFile(t, filepath.Join(dir, "graph", "generated.go")); !strings.Contains(src, `nullv4 "hello/null.v4"`) || !strings.Contains(src, "nullv4.Time") {
		t.Errorf("the mapped type is not imported from hello/null.v4 and named through it:\n%s", src)
	}
	writeTempModule(t, dir)
	goBuild(t, dir)
}

// A generic instantiated over predeclared types names one package, so it is
// one import and an entry can spell it. Requiring a bare identifier after the
// last dot refused database/sql.Null[string], which had generated code that
// compiles.
func TestAModelMayBeAGenericOverPredeclaredTypes(t *testing.T) {
	for _, expr := range []string{"database/sql.Null[string]", "*database/sql.Null[int64]", "[]database/sql.Null[bool]"} {
		if !modelExprOK(expr) {
			t.Errorf("modelExprOK(%q) = false, want it accepted", expr)
		}
	}
	for _, expr := range []string{"database/sql.Null[", "database/sql.Null[]", "database/sql.[string]", "database/sql.Null[string]x", "database/sql.Null[func]"} {
		if modelExprOK(expr) {
			t.Errorf("modelExprOK(%q) = true, want it refused", expr)
		}
	}
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema.graphql": "scalar Note\ninput Range { from: Note to: [Note!] }\ntype Event { at: Note! }\ntype Query { events(in: Range): [Event!]! }\n",
	})
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
		Models: map[string]string{"Note": "database/sql.Null[string]"},
	}); err != nil {
		t.Fatal(err)
	}
	if src := readFile(t, filepath.Join(dir, "graph", "generated.go")); !strings.Contains(src, "sql.Null[string]") {
		t.Errorf("the mapped type is not written as sql.Null[string]:\n%s", src)
	}
	writeTempModule(t, dir)
	goBuild(t, dir)
}

// The root package imports every group package unaliased, beside names of its
// own: the embedded SDL is a package variable called sdl, a federated schema
// imports fed, and NewSchema's parameter is opts. A group of one of those
// names generated a file that did not compile.
func TestAGroupNamedLikeTheRootPackagesOwnNamesStillCompiles(t *testing.T) {
	for _, stem := range []string{"sdl", "opts"} {
		if got := sanitizeGroup(stem, "graph", false); got == stem {
			t.Errorf("sanitizeGroup(%q) = %q, which the generated root package already uses", stem, got)
		}
	}
	// fed is imported only by a federated schema. Renamed everywhere, a
	// schema with a fed.graphql and no federation lost a package it had.
	if got := sanitizeGroup("fed", "graph", true); got == "fed" {
		t.Error("a federated schema's root package imports fed; a group of that name collides")
	}
	if got := sanitizeGroup("fed", "graph", false); got != "fed" {
		t.Errorf("sanitizeGroup(%q) without federation = %q, want it left alone", "fed", got)
	}
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"schema/sdl.graphql":  "type Thing { id: ID! }\ntype Query { things: [Thing!]! }\n",
		"schema/opts.graphql": "type Opt { id: ID! }\nextend type Query { opts: [Opt!]! }\n",
	})
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	goBuild(t, dir)
}

// groupNamedLikeGo is one group per name that used to generate code that did
// not compile, each with a model, an argument, a field the model answers and
// a Resolver of its own, so its package and its model package are both
// imported wherever a group's can be.
func groupNamedLikeGo(names ...string) map[string]string {
	files := map[string]string{
		"schema/account.graphql": "interface Node { id: ID! }\ntype Account implements Node { id: ID! name: String! active: Boolean! n: Int ratio: Float }\ntype Query { account(id: ID!): Account node(id: ID!): Node }\n",
	}
	for i, name := range names {
		t := fmt.Sprintf("Thing%d", i)
		files["schema/"+name+".graphql"] = fmt.Sprintf(
			"type %[1]s implements Node { id: ID! message: String! code: Int ok: Boolean! cause(depth: Int): %[1]s }\ninput %[1]sInput { message: String }\nextend type Query { all%[1]s(first: Int, in: %[1]sInput): [%[1]s!]! }\n", t)
	}
	return files
}

// A group's package is named after its SDL file, and so is its model package,
// which every file using its types imports under that name. error.graphql
// made `error` a package in every file whose Resolver returns one;
// string.graphql, int.graphql and bool.graphql did the same to the types of
// their own fields; append.graphql and nil.graphql to the root's own code;
// init cannot name an import at all; and r and v are what the bindings call
// the Resolver and the parent value.
func TestAGroupNamedLikeSomethingPredeclaredCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	writeFiles(t, dir, groupNamedLikeGo("error", "string", "int", "bool", "any", "append", "nil", "init", "r", "v"))
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	validateGenerated(t, dir, "hello/graph")
}

// A federated root declares more: the source and option SubgraphFS returns,
// and the entities NewSchema takes. A group with no Resolver is registered by
// NewSchema itself, in the scope of all three.
func TestAGroupNamedLikeAFederatedRootsOwnNamesCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	files := map[string]string{"schema/schema.graphql": "type Query { ping: Boolean }\n"}
	for i, name := range []string{"src", "subgraph", "entities", "err", "none"} {
		files["schema/"+name+".graphql"] = fmt.Sprintf("type Leaf%d @key(fields: \"id\") { id: ID! name: String! }\n", i)
	}
	writeFiles(t, dir, files)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph", Federation: true,
	}); err != nil {
		t.Fatal(err)
	}
	validateGenerated(t, dir, "hello/graph")
}

// The reserved names are written down, so this is what keeps them true: every
// identifier the generated code declares that could also be a group's name --
// lower-case letters and digits -- has to be one sanitizeGroup renames. A
// parameter added to an emitter fails here until it is reserved.
func TestEveryNameGeneratedCodeDeclaresIsReserved(t *testing.T) {
	groupName := regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	for name, federation := range map[string]bool{"plain": false, "federated": true} {
		dir := t.TempDir()
		files := groupNamedLikeGo("post", "tag")
		files["schema/leaf.graphql"] = "type Leaf { id: ID! }\nenum Colour { RED }\n"
		files["schema/feed.graphql"] = "type Subscription { ticks(every: Int): Int! posts: Thing0 }\n"
		if federation {
			files["schema/leaf.graphql"] = "type Leaf @key(fields: \"id\") { id: ID! }\nenum Colour { RED }\n"
		}
		writeFiles(t, dir, files)
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph", Federation: federation,
		}); err != nil {
			t.Fatal(err)
		}
		declared := map[string]string{}
		called := map[string]string{}
		err := filepath.WalkDir(filepath.Join(dir, "graph"), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			note := func(ids ...*goast.Ident) {
				for _, id := range ids {
					if id != nil && groupName.MatchString(id.Name) {
						declared[id.Name] = filepath.Base(path)
					}
				}
			}
			goast.Inspect(f, func(n goast.Node) bool {
				switch n := n.(type) {
				case *goast.Field:
					// A struct's fields and an interface's methods are not in
					// any scope a package name is.
					note(n.Names...)
				case *goast.StructType, *goast.InterfaceType:
					return false
				case *goast.CallExpr:
					if id, ok := n.Fun.(*goast.Ident); ok {
						called[id.Name] = filepath.Base(path)
					}
				case *goast.ValueSpec:
					note(n.Names...)
				case *goast.TypeSpec:
					note(n.Name)
				case *goast.FuncDecl:
					if n.Recv == nil {
						note(n.Name)
					}
				case *goast.AssignStmt:
					if n.Tok == token.DEFINE {
						for _, lhs := range n.Lhs {
							id, _ := lhs.(*goast.Ident)
							note(id)
						}
					}
				case *goast.RangeStmt:
					if n.Tok == token.DEFINE {
						key, _ := n.Key.(*goast.Ident)
						value, _ := n.Value.(*goast.Ident)
						note(key, value)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(declared) < 6 {
			t.Fatalf("%s: found only %v; the walk is not reading the generated code", name, declared)
		}
		for _, id := range slices.Sorted(maps.Keys(declared)) {
			if got := sanitizeGroup(id, "graph", federation); got == id {
				t.Errorf("%s: %s declares %s, and a group may still be named that", name, declared[id], id)
			}
		}
		// The built-in functions are reserved one by one, as the emitters
		// come to call them.
		for _, id := range slices.Sorted(maps.Keys(called)) {
			if _, builtin := types.Universe.Lookup(id).(*types.Builtin); builtin && !predeclaredNames[id] {
				t.Errorf("%s: %s calls the built-in %s, and a group may still be named that", name, called[id], id)
			}
		}
	}
}

// The universe's types and constants are all reserved, since generated code
// may write any of them, and its functions only where an emitter calls one:
// a group called delete or min compiled, and asking go/types for every name
// renamed it, and renamed more with each Go that added a built-in. A type or
// constant a new Go adds fails here, where reserving it is a decision.
func TestPredeclaredNamesCoverWhatGeneratedCodeWrites(t *testing.T) {
	for _, name := range types.Universe.Names() {
		switch types.Universe.Lookup(name).(type) {
		case *types.TypeName, *types.Const, *types.Nil:
			if !predeclaredNames[name] {
				t.Errorf("%s is predeclared and a group may still be named that", name)
			}
		}
	}
	for name := range predeclaredNames {
		if types.Universe.Lookup(name) == nil {
			t.Errorf("%s is reserved as predeclared and is not", name)
		}
	}
	for _, name := range []string{"delete", "copy", "min", "max", "print", "close", "new", "len", "make"} {
		if got := sanitizeGroup(name, "graph", true); got != name {
			t.Errorf("sanitizeGroup(%q) = %q; generated code calls no such built-in, and the package had that name", name, got)
		}
	}
}
