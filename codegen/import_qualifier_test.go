package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mapped type's package was referred to by the last element of its import
// path and imported without an alias, which is right only while three things
// hold that nothing checks:
//
//   - the package's name is its directory's name. velox and ent put mutation
//     inputs in .../client/todo under package todoclient, so every reference
//     to todo.CreateTodoInput was undefined;
//   - no two mapped paths end in the same element. .../velox/todo holds the
//     entity's enum and .../velox/client/todo its input; both were written as
//     todo, one import silently won the qualifier map and the other's types
//     resolved against the wrong package;
//   - no mapped path ends in graphql, which the generated file already
//     imports for the engine.
//
// Each qualifier is now decided once per import path and always written as an
// alias, so none of the three can reach the output. Found by the velox + fx
// example, whose first generate did not compile.
func TestMappedPackagesGetTheirOwnQualifier(t *testing.T) {
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

	mk("schema.graphql", `
directive @goModel(model: String) on OBJECT | INPUT_OBJECT | SCALAR | ENUM
enum Status @goModel(model: "hello/entity/todo.Status") { TODO DONE }
input CreateTodoInput @goModel(model: "hello/client/todo.CreateTodoInput") { title: String! status: Status }
input Page @goModel(model: "hello/graphql.Page") { first: Int }
type Todo { title: String! status: Status! }
type Query { todos(page: Page): [Todo!]! }
type Mutation { createTodo(input: CreateTodoInput!): Todo! }
`)
	mk("entity/todo/todo.go", "package todo\n\ntype Status string\n\nconst (\n\tStatusTodo Status = \"TODO\"\n\tStatusDone Status = \"DONE\"\n)\n")
	mk("client/todo/input.go", "package todoclient\n\nimport \"hello/entity/todo\"\n\ntype CreateTodoInput struct {\n\tTitle  string\n\tStatus *todo.Status\n}\n")
	mk("graphql/page.go", "package graphql\n\ntype Page struct {\n\tFirst *int\n}\n")
	writeTempModule(t, dir)

	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	buildGenerated(t, dir)
}

// AutoBind decides whether a Go field answers a GraphQL field by comparing two
// spellings of a type: the Go type rendered with a qualifier, and the
// expression the generator would write. The first used the package's name and
// the second the qualifier above, so once the two could differ, every field
// typed by a renamed package -- entity.Todo.Status in velox -- fell through to
// the Resolver though it is plainly a struct field.
//
// And the reverse: an unmapped package whose name is a mapped package's
// qualifier spells its Status exactly as the mapped one is spelled, so a field
// holding it would bind as if it held the mapped type, which is a compile
// error in the generated file.
func TestAutoBindComparesTypesUnderTheGeneratedQualifier(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
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
	const status = "\n\ntype Status string\n\nconst (\n\tStatusTODO Status = \"TODO\"\n\tStatusDONE Status = \"DONE\"\n)\n"
	mk("schema.graphql", `
directive @goModel(model: String) on OBJECT | INPUT_OBJECT | SCALAR | ENUM
enum Status @goModel(model: "hello/entity/todo.Status") { TODO DONE }
input CreateTodoInput @goModel(model: "hello/client/todo.CreateTodoInput") { title: String! }
type Todo { title: String! status: Status! legacy: Status! }
type Query { todos: [Todo!]! }
type Mutation { createTodo(input: CreateTodoInput!): Todo! }
`)
	mk("entity/todo/todo.go", "package todo"+status)
	// Named after the qualifier entity/todo is given, so the two Status types
	// spell alike unless the unmapped one is told apart by its path.
	mk("legacy/todo/todo.go", "package entitytodo"+status)
	mk("client/todo/input.go", "package todoclient\n\ntype CreateTodoInput struct {\n\tTitle string\n}\n")
	mk("ent/todo.go", `package ent

import (
	"hello/entity/todo"
	legacy "hello/legacy/todo"
)

type Todo struct {
	Title  string
	Status todo.Status
	Legacy legacy.Status
}
`)
	writeTempModule(t, dir)

	src := autoGenerate(t, dir, Config{ModelDirective: ModelDirective{Name: "goModel", Arg: "model"}})
	if !strings.Contains(src, `graphql.Field("status"`) {
		t.Errorf("Todo.status is a struct field of the mapped type and did not bind as one\n%s", src)
	}
	if !strings.Contains(src, "TodoLegacy(") {
		t.Errorf("Todo.legacy holds a different package's Status and must be left to the Resolver\n%s", src)
	}
	buildGenerated(t, dir)
}

// Every qualifier must be a Go identifier nothing else in the file holds, so
// a path element that is not one has to be turned into one.
func TestPathQualifierIsAnUnusedIdentifier(t *testing.T) {
	taken := map[string]string{"todo": "a/todo", "clienttodo": "b/client/todo"}
	for _, c := range []struct{ path, want string }{
		{"example.com/shop/v2", "shop"},       // a major version names no package
		{"gopkg.in/yaml.v3", "yamlv3"},        // the dot is not an identifier rune
		{"example.com/go-kit", "gokit"},       // nor is the dash
		{"example.com/x/graphql", "xgraphql"}, // the engine's own name is reserved
		{"c/client/todo", "todo2"},            // base and parent+base both taken
		{"example.com/123", "pkg"},            // no identifier at all
	} {
		if got := pathQualifier(c.path, taken); got != c.want {
			t.Errorf("pathQualifier(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
