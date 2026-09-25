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

const scaffoldUserSDL = `type User { id: ID! name: String! posts: [Post!]! }
type Query { users: [User!]! }
`

const scaffoldPostSDL = `type Post {
  id: ID!
  title: String!
  "Who wrote it."
  author: User!
}
extend type Query { post(id: ID!): Post }
extend type Mutation { publish(title: String!): Post! }
`

// scaffoldModule lays out a two-group schema in a module and returns the
// directory and a generate function over it.
func scaffoldModule(t *testing.T, scaffold map[string]string) (string, func() error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"schema/user.graphql": scaffoldUserSDL,
		"schema/post.graphql": "type Mutation { noop: Boolean }\n" + scaffoldPostSDL,
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTempModule(t, dir)
	return dir, func() error {
		return Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph",
			Scaffold: scaffold,
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goBuild(t *testing.T, dir string) {
	t.Helper()
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	vet := exec.Command("go", "vet", "./...")
	vet.Dir = dir
	if out, err := vet.CombinedOutput(); err != nil {
		t.Fatalf("go vet: %v\n%s", err, out)
	}
}

// From nothing: the type, the assertion that it satisfies the group's
// Resolver, and a stub for every method, all of which compile.
func TestScaffoldWritesStubsThatCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, generate := scaffoldModule(t, map[string]string{
		"user": "impl.UserResolver",
		"post": "impl.PostResolver",
	})
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	user := readFile(t, filepath.Join(dir, "impl", "user.resolvers.go"))
	post := readFile(t, filepath.Join(dir, "impl", "post.resolvers.go"))
	for _, want := range []string{
		"package impl",
		"type UserResolver struct{}",
		"var _ usergql.Resolver = (*UserResolver)(nil)",
		"func (r *UserResolver) UserPosts(ctx context.Context, obj *",
		`panic("not implemented: User.posts")`,
		"func (r *UserResolver) Users(ctx context.Context)",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user.resolvers.go lacks %q\n%s", want, user)
		}
	}
	for _, want := range []string{
		"args postgql.PostArgs",
		"args postgql.PublishArgs",
		"// Who wrote it.",
		"// PostAuthor resolves Post.author.",
	} {
		if !strings.Contains(post, want) {
			t.Errorf("post.resolvers.go lacks %q\n%s", want, post)
		}
	}
	goBuild(t, dir)
}

// The loop the feature exists for: a method the developer has written is
// never stubbed again or touched, wherever it lives; a field added to the
// SDL arrives as one new stub; and a run with nothing new changes nothing.
func TestScaffoldKeepsWhatExists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, generate := scaffoldModule(t, map[string]string{"user": "impl.UserResolver"})
	handWritten := `package impl

import (
	"context"

	model "hello/graph/model/user"
)

type UserResolver struct{ greeting string }

func (r *UserResolver) Users(context.Context) ([]*model.User, error) { return nil, nil }
`
	if err := os.MkdirAll(filepath.Join(dir, "impl"), 0o755); err != nil {
		t.Fatal(err)
	}
	userGo := filepath.Join(dir, "impl", "user.go")
	if err := os.WriteFile(userGo, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	stubs := filepath.Join(dir, "impl", "user.resolvers.go")
	got := readFile(t, stubs)
	if strings.Contains(got, ") Users(") || strings.Contains(got, "type UserResolver") {
		t.Fatalf("scaffolded what user.go already has:\n%s", got)
	}
	if !strings.Contains(got, ") UserPosts(") {
		t.Fatalf("UserPosts was not scaffolded:\n%s", got)
	}
	if readFile(t, userGo) != handWritten {
		t.Fatal("scaffolding edited a hand-written file")
	}

	// The developer fills the stub in, then adds a field to the SDL.
	filled := strings.Replace(got, `panic("not implemented: User.posts")`, "return nil, nil // written by hand", 1)
	if err := os.WriteFile(stubs, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	sdl := filepath.Join(dir, "schema", "user.graphql")
	if err := os.WriteFile(sdl, []byte(strings.Replace(scaffoldUserSDL, "posts: [Post!]!", "posts: [Post!]! friends: [User!]!", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	again := readFile(t, stubs)
	if !strings.Contains(again, "// written by hand") {
		t.Fatalf("regenerating lost a filled-in stub:\n%s", again)
	}
	if !strings.Contains(again, ") UserFriends(") || strings.Count(again, ") UserPosts(") != 1 {
		t.Fatalf("want UserFriends added and UserPosts once:\n%s", again)
	}
	goBuild(t, dir)

	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if readFile(t, stubs) != again {
		t.Fatal("a run with nothing new changed the stub file")
	}
}

// A misspelt group would otherwise scaffold nothing and say nothing.
func TestScaffoldRejectsAnUnknownGroup(t *testing.T) {
	_, generate := scaffoldModule(t, map[string]string{"usr": "impl.UserResolver"})
	err := generate()
	if err == nil || !strings.Contains(err.Error(), `"usr" is not a group`) || !strings.Contains(err.Error(), "post, user") {
		t.Fatalf("err = %v, want one naming the group and listing the real ones", err)
	}
}

// With one group there is no group name; the output package names it.
func TestScaffoldFlatUsesThePackageName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte("type Query { hello: String! }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"graph": "server.Resolver"},
	}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, "server", "graph.resolvers.go"))
	for _, want := range []string{`graphgql "hello/graph"`, "var _ graphgql.Resolver = (*Resolver)(nil)", ") Hello(ctx context.Context) (string, error)"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q\n%s", want, got)
		}
	}
}

// A field removed from the SDL leaves its method behind, and it compiles:
// the var _ Resolver assertion checks only what the interface asks for. The
// run that removed it is the one moment anything knows, so it says so.
// Unexported methods are the implementation's own helpers and are not named.
func TestScaffoldNamesMethodsTheResolverNoLongerHas(t *testing.T) {
	dir, _ := scaffoldModule(t, nil)
	handWritten := `package impl

import "context"

type UserResolver struct{}

func (r *UserResolver) UserFriends(context.Context) error { return nil }
func (r *UserResolver) Users(context.Context) error      { return nil }
func (r *UserResolver) cache()                          {}
func (UserResolver) Archived()                          {}
`
	if err := os.MkdirAll(filepath.Join(dir, "impl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "impl", "user.go"), []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	var notes []string
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"user": "impl.UserResolver"},
		Notef:    func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var stale []string
	for _, n := range notes {
		if strings.Contains(n, "UserResolver") {
			stale = append(stale, n)
		}
	}
	want := "scaffold user: impl.UserResolver has methods the user Resolver does not: Archived, UserFriends. If"
	if len(stale) != 1 || !strings.HasPrefix(stale[0], want) {
		t.Fatalf("notes = %q\nwant one starting %q", notes, want)
	}
}
