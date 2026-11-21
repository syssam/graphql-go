package codegen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A scaffolded stub compiles, and a suite that never selects its field stays
// green: a consumer moving from gqlgen found 18 of them by hand. A generate
// names what is left after every run, and CheckStubs makes a check fail on
// it, which is what a CI job can hold.
func TestScaffoldedStubsAreNamedAndCheckStubsFailsOnThem(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	var notes []string
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"user": "impl.UserResolver", "post": "impl.PostResolver"},
		Notef:    func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	stubNote, count := "", 0
	for _, n := range notes {
		if strings.Contains(n, "still hold the panic") {
			stubNote, count = n, count+1
		}
	}
	// One note for the run, both groups in it.
	if count != 1 {
		t.Errorf("%d stub notes, want one for the whole run; notes:\n%s", count, strings.Join(notes, "\n"))
	}
	for _, want := range []string{"Query.users", "User.posts", "Post.author"} {
		if !strings.Contains(stubNote, want) {
			t.Errorf("the generate that wrote the stubs does not name %s; notes:\n%s", want, strings.Join(notes, "\n"))
		}
	}

	// One stub implemented; one begun by its author, which is theirs now.
	userFile := filepath.Join(dir, "impl", "user.resolvers.go")
	edit := func(path, from, to string) {
		t.Helper()
		src := readFile(t, path)
		if !strings.Contains(src, from) {
			t.Fatalf("%s holds no %s", path, from)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(src, from, to, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	postFile := filepath.Join(dir, "impl", "post.resolvers.go")
	edit(userFile, `panic("not implemented: User.posts")`, `return nil, nil`)
	edit(postFile, `panic("not implemented: Mutation.publish")`, "_ = args\n\tpanic(\"not implemented: Mutation.publish\")")
	edit(postFile, `panic("not implemented: Query.post")`, "panic(\"not implemented: Query.post\")\n\t_ = args")

	check := cfg
	check.Notef = nil
	err := Check(context.Background(), check, CheckOptions{Stubs: true})
	if !errors.Is(err, ErrStubs) || errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStubs alone", err)
	}
	for _, want := range []string{"Query.users (impl.UserResolver, impl/user.resolvers.go)", "Post.author (impl.PostResolver, impl/post.resolvers.go)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the check does not name %q:\n%v", want, err)
		}
	}
	for _, gone := range []string{"User.posts", "Mutation.publish", "Query.post"} {
		if strings.Contains(err.Error(), gone) {
			t.Errorf("the check names %s, which is no longer the scaffolded body:\n%v", gone, err)
		}
	}

	// Without Stubs the same tree is current.
	if err := Check(context.Background(), check, CheckOptions{}); err != nil {
		t.Fatalf("a check without Stubs: %v", err)
	}

	stub := regexp.MustCompile(`panic\("not implemented: [^"]*"\)`)
	for _, f := range []string{userFile, postFile} {
		if err := os.WriteFile(f, stub.ReplaceAll([]byte(readFile(t, f)), []byte(`panic("todo")`)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Generate(context.Background(), check); err != nil {
		t.Fatalf("every stub replaced: %v", err)
	}
	notes = nil
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if strings.Contains(n, "still hold the panic") {
			t.Errorf("a generate with no stub left names one: %s", n)
		}
	}
}

func TestCheckStubsNeedsScaffold(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	err := Check(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
	}, CheckOptions{Stubs: true})
	if err == nil || !strings.Contains(err.Error(), "Scaffold names none") {
		t.Fatalf("err = %v, want a refusal naming Scaffold", err)
	}
}

// A project with hundreds of stubs reads one line, not all of them.
func TestTheStubNoteIsCapped(t *testing.T) {
	t.Parallel()
	var said string
	b := &builder{cfg: Config{Notef: func(format string, args ...any) { said = fmt.Sprintf(format, args...) }}}
	stubs := map[string]bool{}
	for i := range 12 {
		stubs[fmt.Sprintf("Query.f%02d", i)] = true
	}
	b.reportStubs(stubs)
	if !strings.Contains(said, "12 Resolver method(s)") || !strings.Contains(said, "Query.f09 and 2 more") || strings.Contains(said, "Query.f10") {
		t.Errorf("note = %q", said)
	}
}

// Two spellings of one target are one implementation, and each of its stubs
// is listed once, not once per spelling.
func TestCheckStubsListsAStubOnceWhateverTheSpelling(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"user": "impl.Resolver", "post": "./impl/.Resolver"},
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	err := Check(context.Background(), cfg, CheckOptions{Stubs: true})
	if !errors.Is(err, ErrStubs) {
		t.Fatalf("err = %v, want ErrStubs", err)
	}
	if n := strings.Count(err.Error(), "Query.users ("); n != 1 {
		t.Errorf("Query.users is listed %d times:\n%v", n, err)
	}
}

// Where case matters, Impl and impl are two packages and each holds its own
// stubs. checkScaffoldTargets read a directory once per lower-cased spelling,
// so the second was never read: its stubs went unreported by -check -stubs,
// and a generated name declared in it was never refused. Skipped where case
// is ignored, since there they are one directory.
func TestCheckStubsReadsEverySpellingWhereCaseMatters(t *testing.T) {
	t.Parallel()
	dir, _ := scaffoldModule(t, nil)
	if caseInsensitive(t, dir) {
		t.Skip("case-insensitive filesystem: impl and Impl are one directory")
	}
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"user": "impl.UserR", "post": "Impl.PostR"},
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	err := Check(context.Background(), cfg, CheckOptions{Stubs: true})
	if !errors.Is(err, ErrStubs) {
		t.Fatalf("err = %v, want ErrStubs", err)
	}
	for _, want := range []string{"Query.users (impl.UserR", "Query.post (Impl.PostR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the stubs lack %q:\n%v", want, err)
		}
	}
}
