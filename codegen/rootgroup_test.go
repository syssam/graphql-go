package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A root is the one type every group adds to. Grouped as a type, all of a
// large schema's root resolvers landed in whichever group declared Query --
// one package holding thousands of methods, the monolith grouping exists to
// split. Each root field now goes to the group of the file declaring it, and
// the engine merges the groups' Query bindings back into one root.
func TestRootFieldsAreGroupedByTheirOwnFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("schema/root.graphql", "type Query { version: String! }\ntype Mutation { ping: String! }\n")
	write("schema/user.graphql", "type User { id: ID! name: String! }\nextend type Query { users(first: Int): [User!]! }\n")
	write("schema/post.graphql", "type Post { id: ID! title: String! }\nextend type Query { posts: [Post!]! }\nextend type Mutation { publish(id: ID!): Post! }\n")
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}

	read := func(g string) string {
		b, err := os.ReadFile(filepath.Join(dir, "graph", g, "generated.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	root, user, post := read("root"), read("user"), read("post")
	for _, c := range []struct {
		src, group, want string
		in               bool
	}{
		{root, "root", "Version(ctx context.Context)", true},
		{root, "root", "Ping(ctx context.Context)", true},
		{root, "root", "Users(", false},
		{root, "root", "Posts(", false},
		{user, "user", "Users(ctx context.Context, args UsersArgs)", true},
		{user, "user", "type UsersArgs struct", true},
		{user, "user", "graphql.Args[UsersArgs]()", true},
		{post, "post", "Posts(ctx context.Context)", true},
		{post, "post", "Publish(ctx context.Context, args PublishArgs)", true},
		{post, "post", "graphql.Mutation(", true},
		{user, "user", "graphql.Mutation(", false},
	} {
		if strings.Contains(c.src, c.want) != c.in {
			t.Errorf("%s group: contains %q = %v, want %v\n%s", c.group, c.want, !c.in, c.in, c.src)
		}
	}

	// Three groups each bind part of Query; one request reads all three.
	write("graph/stub_test.go", `package graph_test

import (
	"context"
	"testing"

	"github.com/syssam/graphql-go"
	"hello/graph"
	"hello/graph/model/post"
	"hello/graph/model/user"
	postg "hello/graph/post"
	rootg "hello/graph/root"
	userg "hello/graph/user"
)

type rootR struct{}

func (rootR) Version(context.Context) (string, error) { return "1", nil }
func (rootR) Ping(context.Context) (string, error)    { return "pong", nil }

type userR struct{}

func (userR) Users(context.Context, userg.UsersArgs) ([]*user.User, error) {
	return []*user.User{{ID: "u1", Name: "Ada"}}, nil
}

type postR struct{}

func (postR) Posts(context.Context) ([]*post.Post, error) { return []*post.Post{{ID: "p1", Title: "Hi"}}, nil }
func (postR) Publish(context.Context, postg.PublishArgs) (*post.Post, error) {
	return &post.Post{ID: "p1", Title: "Hi"}, nil
}

func TestRootMergesAcrossGroups(t *testing.T) {
	s, err := graph.NewSchema(rootg.Bindings(rootR{}), userg.Bindings(userR{}), postg.Bindings(postR{}))
	if err != nil {
		t.Fatal(err)
	}
	resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: "{ version users { name } posts { title } }"})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors[0])
	}
	if want := `+"`"+`{"version":"1","users":[{"name":"Ada"}],"posts":[{"title":"Hi"}]}`+"`"+`; string(resp.Data) != want {
		t.Fatalf("data = %s", resp.Data)
	}
}
`)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	run := exec.Command("go", "test", "-count=1", "-run", "TestRootMergesAcrossGroups", "-v", "./graph")
	run.Dir = dir
	out, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestRootMergesAcrossGroups") {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

// velox declares every root field in one shared file, so grouping by file puts
// them all in one group again. RootFieldGroup can follow the returned type
// instead; a field returning a built-in scalar has no such group and falls
// back to its file.
func TestRootFieldGroupCanFollowTheReturnedType(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"schema.graphql":     "type Query { todos: [Todo!]! users: [User!]! version: String! }\ntype Mutation { createTodo(title: String!): Todo! }\n",
		"velox_todo.graphql": "type Todo { id: ID! title: String! }\n",
		"velox_user.graphql": "type User { id: ID! name: String! }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var seen []RootField
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"*.graphql"},
		Output: "graph", Package: "hello/graph",
		GroupFunc: func(_, file string) string { return strings.TrimPrefix(fileStem(file), "velox_") },
		RootFieldGroup: func(f RootField) string {
			seen = append(seen, f)
			return f.ReturnGroup
		},
	}); err != nil {
		t.Fatal(err)
	}
	read := func(g string) string {
		b, err := os.ReadFile(filepath.Join(dir, "graph", g, "generated.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	todo, user, types := read("todo"), read("user"), read("types")
	if !strings.Contains(todo, "Todos(ctx") || !strings.Contains(todo, "CreateTodo(ctx") {
		t.Errorf("todo group lacks the root fields returning Todo\n%s", todo)
	}
	if !strings.Contains(user, "Users(ctx") {
		t.Errorf("user group lacks Query.users\n%s", user)
	}
	if !strings.Contains(types, "Version(ctx") || strings.Contains(types, "Todos(ctx") {
		t.Errorf("only the scalar-returning field belongs to the shared file's group\n%s", types)
	}
	for _, f := range seen {
		if f.Root == "Query" && f.Name == "todos" && (f.ReturnType != "Todo" || f.ReturnGroup != "todo" || filepath.Base(f.SDLFile) != "schema.graphql") {
			t.Errorf("RootFieldGroup was told %+v for Query.todos", f)
		}
	}
}
