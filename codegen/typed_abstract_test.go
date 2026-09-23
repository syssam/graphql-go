package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const typedAbstractSDL = `
interface Node { id: ID! }
interface Named implements Node { id: ID! name: String! }
type User implements Node & Named { id: ID! name: String! }
type Post implements Node { id: ID! title: String! }
union SearchResult = User | Post
type Query { node(id: ID!): Node named: Named search(q: String!): [SearchResult!]! }
`

// An interface or union whose members are all generated models gets a Go
// marker interface, so the Resolver says what it may return. It used to be
// any, which accepted a value of a type the schema does not allow and moved
// the error from the compiler to a request.
func TestAbstractTypesOfGeneratedModelsAreTyped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(typedAbstractSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	src := autoGenerate(t, dir, Config{AutoBind: []string{}})
	for _, want := range []string{
		"Node(ctx context.Context, args NodeArgs) (model.Node, error)",
		"Named(ctx context.Context) (model.Named, error)",
		"Search(ctx context.Context, args SearchArgs) ([]model.SearchResult, error)",
		`graphql.Interface[model.Node]("Node")`,
		`graphql.Union[model.SearchResult]("SearchResult")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go lacks %s\n%s", want, src)
		}
	}
	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type Node interface {\n\tIsNode()\n}",
		// Named implements Node, so a Named is usable where a Node is asked for.
		"type Named interface {\n\tIsNode()\n\tIsNamed()\n}",
		"type SearchResult interface {\n\tIsSearchResult()\n}",
		"func (*User) IsNode()",
		"func (*User) IsNamed()",
		"func (*User) IsSearchResult()",
		"func (*Post) IsSearchResult()",
	} {
		if !strings.Contains(string(models), want) {
			t.Errorf("models.go lacks %q\n%s", want, models)
		}
	}
	if strings.Contains(string(models), "func (*Post) IsNamed()") {
		t.Error("Post was given a marker for an interface it does not implement")
	}

	if testing.Short() {
		return
	}
	stub := `package graph

import (
	"context"

	"hello/graph/model"
)

type Stub struct{}

func (Stub) Node(context.Context, NodeArgs) (model.Node, error) {
	return &model.Post{ID: "p1", Title: "Hello"}, nil
}

func (Stub) Named(context.Context) (model.Named, error) {
	return &model.User{ID: "u1", Name: "Ada"}, nil
}

func (Stub) Search(context.Context, SearchArgs) ([]model.SearchResult, error) {
	return []model.SearchResult{&model.User{ID: "u1", Name: "Ada"}, &model.Post{ID: "p1", Title: "Hello"}}, nil
}
`
	testSrc := `package graph_test

import (
	"context"
	"testing"

	"github.com/syssam/graphql-go"
	"hello/graph"
)

func TestServes(t *testing.T) {
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: ` + "`" + `{
		node(id: "p1") { id ... on Post { title } }
		named { name }
		search(q: "x") { __typename ... on User { name } ... on Post { title } }
	}` + "`" + `})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	want := ` + "`" + `{"node":{"id":"p1","title":"Hello"},"named":{"name":"Ada"},"search":[{"__typename":"User","name":"Ada"},{"__typename":"Post","title":"Hello"}]}` + "`" + `
	if got := string(resp.Data); got != want {
		t.Fatalf("data = %s\nwant %s", got, want)
	}
}
`
	for name, body := range map[string]string{"stub.go": stub, "serve_test.go": testSrc} {
		if err := os.WriteFile(filepath.Join(dir, "graph", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	run := exec.Command("go", "test", "-count=1", "./graph")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("the generated schema does not serve: %v\n%s", err, out)
	}
}

// A marker method can only be declared on a type this generator writes, so an
// abstract type with a mapped member stays any rather than generating a method
// on someone else's type.
func TestAbstractTypeWithAMappedMemberStaysAny(t *testing.T) {
	dir := t.TempDir()
	const sdl = `
type User { id: ID! }
type Post { id: ID! }
union SearchResult = User | Post
type Query { search: [SearchResult!]! }
`
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	src := autoGenerate(t, dir, Config{AutoBind: []string{}, Models: map[string]string{"Post": "example.com/ext.Post"}})
	for _, want := range []string{"Search(ctx context.Context) ([]any, error)", `graphql.Union[any]("SearchResult")`} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go lacks %s\n%s", want, src)
		}
	}
}

// A member with a field whose Go name is the marker method would declare
// IsNode twice, once as a field and once as a method, which does not compile.
func TestAbstractTypeWhoseMethodNameIsTakenStaysAny(t *testing.T) {
	dir := t.TempDir()
	const sdl = `
interface Node { id: ID! }
type User implements Node { id: ID! isNode: Boolean! }
type Query { node: Node }
`
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	src := autoGenerate(t, dir, Config{AutoBind: []string{}})
	if !strings.Contains(src, "Node(ctx context.Context) (any, error)") {
		t.Errorf("Node was typed although User has an isNode field\n%s", src)
	}
}
