package codegen

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const helloSDL = `
type User { id: ID! name: String! }
type Query { user(id: ID!): User users: [User!]! }
`

func TestGenerateHelloFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(helloSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(bindings)
	for _, want := range []string{
		`graphql.Object[model.User]("User"`,
		`graphql.Field("id"`,
		`graphql.Field("name"`,
		`graphql.Query(`,
		`graphql.ResolveArgs("user"`,
		`graphql.Resolve("users"`,
		`graphql.Args[UserArgs]()`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go missing %s\n%s", want, src)
		}
	}
	for _, rel := range []string{"schema.go", "model/models.go", "schema/schema.graphql"} {
		p := filepath.Join(dir, "graph", filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, filepath.Join(dir, "graph", "generated.go"), bindings, 0); err != nil {
		t.Fatalf("generated.go does not parse: %v", err)
	}
}

func writeTempModule(t *testing.T, dir string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(thisFile))
	mod := `module hello

go 1.27

require github.com/syssam/graphql-go v0.0.0

require github.com/vektah/gqlparser/v2 v2.5.37

replace github.com/syssam/graphql-go => ` + filepath.ToSlash(root) + `
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedHelloExecutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(helloSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	stub := `package graph

import (
	"context"
	"hello/graph/model"
)

type Stub struct{}

func (Stub) User(_ context.Context, a UserArgs) (*model.User, error) {
	return &model.User{ID: a.ID, Name: "Ada"}, nil
}

func (Stub) Users(context.Context) ([]*model.User, error) {
	u, err := (Stub{}).User(context.Background(), UserArgs{ID: "1"})
	return []*model.User{u}, err
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package graph_test

import (
	"context"
	"hello/graph"
	"testing"

	"github.com/syssam/graphql-go"
)

func TestUserQuery(t *testing.T) {
	s, err := graph.NewSchema(graph.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	q := "{ user(id: \"1\") { id name } }"
	resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: q})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors[0])
	}
	want := "{\"user\":{\"id\":\"1\",\"name\":\"Ada\"}}"
	if string(resp.Data) != want {
		t.Fatalf("data = %s", resp.Data)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "exec_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "-count=1", "./graph")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

func TestGenerateEmptyGlobs(t *testing.T) {
	if err := Generate(context.Background(), Config{Output: "x", Package: "x"}); err == nil {
		t.Fatal("expected error")
	}
}

const richSDL = `
scalar Time
enum Role { ADMIN USER }
input PostFilter { authorId: ID tag: String }
input UpdatePostInput { title: String body: String }
type User { id: ID! name: String! role: Role! createdAt: Time! posts(first: Int): [Post!]! }
type Post { id: ID! title: String! author: User! publishedAt: Time }
type Query { user(id: ID!): User posts(filter: PostFilter): [Post!]! }
`

func TestGenerateRichFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(richSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Generate(context.Background(), Config{
		Dir:                    dir,
		SchemaGlobs:            []string{"schema.graphql"},
		Output:                 "graph",
		Package:                "blog/graph",
		Models:                 map[string]string{"Time": "time.Time"},
		NullableInputOmittable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(bindings)
	for _, want := range []string{
		`"time"`,
		`graphql.Enum[model.Role]("Role"`,
		`model.RoleAdmin: "ADMIN"`,
		`graphql.Input[model.PostFilter]("PostFilter")`,
		`graphql.Input[model.UpdatePostInput]("UpdatePostInput")`,
		`graphql.Resolve("author"`,
		`graphql.ResolveArgs("posts"`,
		`graphql.Args[UserPostsArgs]()`,
		`func(v *model.User) time.Time`,
		`func(v *model.Post) *time.Time`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go missing %s\n%s", want, src)
		}
	}
	if strings.Contains(src, "__type") || strings.Contains(src, "__schema") {
		t.Errorf("introspection fields leaked into generated.go\n%s", src)
	}
	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	ms := string(models)
	for _, want := range []string{
		`type Role string`,
		`RoleAdmin Role = "ADMIN"`,
		`CreatedAt time.Time`,
		`PublishedAt *time.Time`,
		`AuthorID graphql.Omittable[*graphql.ID]`,
		`graphql.Omittable[*string]`,
	} {
		if !strings.Contains(ms, want) {
			t.Errorf("models.go missing %s\n%s", want, ms)
		}
	}
	if strings.Contains(ms, "type Time ") {
		t.Errorf("mapped scalar Time should not emit a model type\n%s", ms)
	}
	res, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	rs := string(res)
	for _, want := range []string{
		`UserPosts(ctx context.Context, obj *model.User, args UserPostsArgs)`,
		`PostAuthor(ctx context.Context, obj *model.Post)`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("generated.go missing %s\n%s", want, rs)
		}
	}
	args, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	as := string(args)
	for _, want := range []string{
		`type UserPostsArgs struct`,
		`First *int`,
		`Filter *model.PostFilter`,
	} {
		if !strings.Contains(as, want) {
			t.Errorf("generated.go missing %s\n%s", want, as)
		}
	}
	if strings.Contains(as, "Omittable") {
		t.Errorf("field arguments should not use Omittable\n%s", as)
	}
}

func TestGenerateTwoGroups(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "user.graphql"), []byte(`
type User { id: ID! name: String! posts: [Post!]! }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "post.graphql"), []byte(`
type Post { id: ID! title: String! author: User! }
type Query { users: [User!]! user(id: ID!): User }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"*.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "generated.go")); err == nil {
		t.Fatal("root generated.go should not exist when there are multiple groups")
	}
	userBind, err := os.ReadFile(filepath.Join(dir, "graph", "user", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	us := string(userBind)
	for _, want := range []string{
		`package user`,
		`graphql.Object[user.User]("User"`,
		`graphql.Resolve("posts"`,
		`func Bindings(r Resolver)`,
	} {
		if !strings.Contains(us, want) {
			t.Errorf("user/generated.go missing %s\n%s", want, us)
		}
	}
	if strings.Contains(us, "graphql.Query") {
		t.Errorf("Query bindings leaked into user group\n%s", us)
	}
	postBind, err := os.ReadFile(filepath.Join(dir, "graph", "post", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	ps := string(postBind)
	for _, want := range []string{
		`package post`,
		`graphql.Object[post.Post]("Post"`,
		`graphql.Query(`,
		`graphql.Resolve("users"`,
		`graphql.ResolveArgs("user"`,
		`graphql.Args[UserArgs]()`,
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("post/generated.go missing %s\n%s", want, ps)
		}
	}
	sch, err := os.ReadFile(filepath.Join(dir, "graph", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	ss := string(sch)
	for _, want := range []string{
		`type Resolvers struct`,
		`User user.Resolver`,
		`Post post.Resolver`,
		`user.Bindings(r.User)`,
		`post.Bindings(r.Post)`,
		`func NewSchema(r Resolvers`,
		`"hello/graph/user"`,
		`"hello/graph/post"`,
	} {
		if !strings.Contains(ss, want) {
			t.Errorf("schema.go missing %s\n%s", want, ss)
		}
	}
	resUser, err := os.ReadFile(filepath.Join(dir, "graph", "user", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resUser), "UserPosts(") {
		t.Errorf("user/generated.go missing UserPosts\n%s", resUser)
	}
	resPost, err := os.ReadFile(filepath.Join(dir, "graph", "post", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	rp := string(resPost)
	for _, want := range []string{"PostAuthor(", "Users(", "User("} {
		if !strings.Contains(rp, want) {
			t.Errorf("post/generated.go missing %s\n%s", want, rp)
		}
	}
}

func TestGenerateGroupFuncOmitsPureGroupFromResolvers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(helloSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
		GroupFunc: func(typeName, _ string) string {
			if typeName == "User" {
				return "user"
			}
			return "query"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	userBind, err := os.ReadFile(filepath.Join(dir, "graph", "user", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(userBind), "func Bindings()") {
		t.Fatalf("pure user group should expose Bindings() with no Resolver\n%s", userBind)
	}
	if strings.Contains(string(userBind), "type Resolver interface") {
		t.Fatalf("pure user group should not declare a Resolver\n%s", userBind)
	}
	sch, err := os.ReadFile(filepath.Join(dir, "graph", "schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	ss := string(sch)
	if !strings.Contains(ss, "Query query.Resolver") {
		t.Errorf("schema.go missing Query resolver field\n%s", ss)
	}
	if strings.Contains(ss, "User user.Resolver") {
		t.Errorf("pure user group should be omitted from Resolvers\n%s", ss)
	}
	if !strings.Contains(ss, "user.Bindings()") {
		t.Errorf("schema.go missing user.Bindings()\n%s", ss)
	}
}

func TestGeneratedTwoGroupsExecutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "user.graphql"), []byte(`
type User { id: ID! name: String! posts: [Post!]! }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "post.graphql"), []byte(`
type Post { id: ID! title: String! author: User! }
type Query { users: [User!]! }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"*.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}
	writeTempModule(t, dir)
	userStub := `package user

import (
	"context"

	"hello/graph/model/post"
	"hello/graph/model/user"
)

type Stub struct{}

func (Stub) UserPosts(context.Context, *user.User) ([]*post.Post, error) {
	return nil, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "user", "stub.go"), []byte(userStub), 0o644); err != nil {
		t.Fatal(err)
	}
	postStub := `package post

import (
	"context"

	"hello/graph/model/post"
	"hello/graph/model/user"
)

type Stub struct{}

func (Stub) Users(context.Context) ([]*user.User, error) {
	return []*user.User{{ID: "1", Name: "Ada"}}, nil
}

func (Stub) PostAuthor(_ context.Context, _ *post.Post) (*user.User, error) {
	return &user.User{ID: "1", Name: "Ada"}, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "post", "stub.go"), []byte(postStub), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package graph_test

import (
	"context"
	"hello/graph"
	"hello/graph/post"
	"hello/graph/user"
	"testing"

	"github.com/syssam/graphql-go"
)

func TestUsersQuery(t *testing.T) {
	s, err := graph.NewSchema(graph.Resolvers{Post: post.Stub{}, User: user.Stub{}})
	if err != nil {
		t.Fatal(err)
	}
	resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{Query: "{ users { id name } }"})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors[0])
	}
	want := "{\"users\":[{\"id\":\"1\",\"name\":\"Ada\"}]}"
	if string(resp.Data) != want {
		t.Fatalf("data = %s", resp.Data)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "graph", "exec_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "-count=1", "./graph")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

// rootScalarSDL has an argument-less scalar field on each root type. A root
// object's parent is graphql.Root, which carries no data, so these must be
// resolver methods; emitting them as pure fields produces an accessor on a
// model type that is never generated.
const rootScalarSDL = `
type User { id: ID! name: String! }
type Query { version: String! user(id: ID!): User }
type Mutation { ping: Boolean! }
`

func TestGenerateRootScalarFieldsAreResolvers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(rootScalarSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, bad := range []string{"model.Query", "model.Mutation"} {
		if strings.Contains(got, bad) {
			t.Errorf("bindings reference %s, which is never generated:\n%s", bad, got)
		}
	}

	r, err := os.ReadFile(filepath.Join(dir, "graph", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Version(", "Ping("} {
		if !strings.Contains(string(r), want) {
			t.Errorf("Resolver interface missing %s:\n%s", want, r)
		}
	}
}

// TestModelCycleFallsBackToOnePackage covers the case per-group model
// packages cannot express: input objects in two groups that reference each
// other. Go forbids the import cycle, so the models must stay in one package.
func TestModelCycleFallsBackToOnePackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.graphql"), []byte(`
type Alpha { id: ID! }
input AlphaFilter { name: String, beta: BetaFilter }
type Query { alpha(f: AlphaFilter): Alpha }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beta.graphql"), []byte(`
type Beta { id: ID! }
input BetaFilter { name: String, alpha: AlphaFilter }
extend type Query { beta(f: BetaFilter): Beta }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"*.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "graph", "model", "models.go")); err != nil {
		t.Fatalf("cyclic inputs must fall back to one model package: %v", err)
	}
	for _, g := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(dir, "graph", "model", g)); err == nil {
			t.Errorf("model/%s was emitted despite the input cycle", g)
		}
	}

	// Bindings are still split per group; only the models are shared.
	for _, g := range []string{"alpha", "beta"} {
		b, err := os.ReadFile(filepath.Join(dir, "graph", g, "generated.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "model.") {
			t.Errorf("%s/bindings.go should reference the shared model package:\n%s", g, b)
		}
	}
}

// TestModelsMapToExternalPackage covers a Config.Models entry whose Go type
// lives in a package the generator does not already import. The whole import
// path used to be written inline, which is not valid Go, so only types from
// packages that happened to be imported already (time) worked.
func TestModelsMapToExternalPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(`
type Product { id: ID! name: String! }
type Query { product(id: ID!): Product }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
		Models:      map[string]string{"Product": "example.com/x/shop.Product"},
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	for _, name := range []string{"generated.go"} {
		b, err := os.ReadFile(filepath.Join(dir, "graph", name))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "*example.com/x/shop.Product") {
			t.Errorf("%s writes the import path inline:\n%s", name, src)
		}
		if !strings.Contains(src, `"example.com/x/shop"`) {
			t.Errorf("%s is missing the import for the mapped model:\n%s", name, src)
		}
		if !strings.Contains(src, "shop.Product") {
			t.Errorf("%s does not reference shop.Product:\n%s", name, src)
		}
	}
}

func TestSplitModelExpr(t *testing.T) {
	for _, tc := range []struct{ expr, path, ref string }{
		{"time.Time", "time", "time.Time"},
		{"example.com/x/shop.Product", "example.com/x/shop", "shop.Product"},
		{"*example.com/x/shop.Product", "example.com/x/shop", "*shop.Product"},
		{"string", "", "string"},
		{"[]byte", "", "[]byte"},
	} {
		path, ref := splitModelExpr(tc.expr)
		if path != tc.path || ref != tc.ref {
			t.Errorf("splitModelExpr(%q) = (%q, %q), want (%q, %q)", tc.expr, path, ref, tc.path, tc.ref)
		}
	}
}
