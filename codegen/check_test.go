package codegen

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshot is every file under dir, by path relative to it, with its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = string(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Config.Check is a generate that writes nothing and says what it would
// have done: CI's "is the generated code current" without running the
// generator over the tree and asking git, and the same question asked
// locally before a commit.
func TestCheckReportsWhatAGenerateWouldChange(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireFieldModule(t, scaffold, wireFieldDeps)
	cfg := Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"},
	}
	check := func(t *testing.T) (map[string]string, error) {
		t.Helper()
		before := snapshot(t, dir)
		c := cfg
		c.Check = true
		err := Generate(context.Background(), c)
		if after := snapshot(t, dir); len(after) != len(before) {
			t.Fatalf("a check changed the tree: %d files before, %d after", len(before), len(after))
		} else {
			for path, content := range before {
				if after[path] != content {
					t.Fatalf("a check wrote %s", path)
				}
			}
		}
		return before, err
	}
	stale := func(t *testing.T, err error, want ...string) {
		t.Helper()
		if !errors.Is(err, ErrStale) {
			t.Fatalf("err = %v, want ErrStale", err)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the report does not name %s:\n%v", w, err)
			}
		}
	}

	// Nothing generated yet: everything would be created, and nothing
	// rewritten.
	_, err := check(t)
	stale(t, err, "create graph/wire.go", "create graph/user/generated.go", "create graph/schema/user.graphql")
	if strings.Contains(err.Error(), "rewrite") {
		t.Errorf("a file that does not exist yet was reported as rewritten too:\n%v", err)
	}

	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := check(t); err != nil {
		t.Fatalf("just generated, and the check says: %v", err)
	}

	// The schema moves on: the group it touched, and nothing else.
	sdl := filepath.Join(dir, "schema", "user.graphql")
	if err := os.WriteFile(sdl, []byte(strings.Replace(readFile(t, sdl), "type User {", "type User {\n  nickname: String", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = check(t)
	stale(t, err, "graph/model/user/models.go", "graph/schema/user.graphql")
	if strings.Contains(err.Error(), "post") {
		t.Errorf("a change to the user group reported the post group's files:\n%v", err)
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// A generated file someone edited, and one this run no longer writes.
	gen := filepath.Join(dir, "graph", "post", "generated.go")
	if err := os.WriteFile(gen, []byte(readFile(t, gen)+"\n// edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, "graph", "old", "generated.go")
	writeFiles(t, dir, map[string]string{
		"graph/old/generated.go":   generatedHeader + "\n\npackage old\n",
		"graph/schema/old.graphql": "type Old { id: ID! }\n",
	})
	_, err = check(t)
	stale(t, err, "rewrite graph/post/generated.go", "delete graph/old/generated.go", "delete graph/schema/old.graphql")
	if _, serr := os.Stat(leftover); serr != nil {
		t.Error("a check deleted the leftover file")
	}

	// Scaffolding adds to code the author owns, and a stub it has not added
	// is a compile error already; a check is about what gqlc owns.
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "graph", "user", "user.resolvers.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := check(t); err != nil {
		t.Errorf("a missing resolvers file, which is the author's, was reported: %v", err)
	}
}

// A check refuses what a generate refuses, so a configuration error is not
// reported as the code being current.
func TestCheckRefusesWhatGenerateRefuses(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireFieldModule(t, scaffold, wireFieldDeps)
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps", Pass: map[string]string{"nobody": "d"}},
		Check: true,
	})
	if err == nil || errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("err = %v, want the configuration refused, not a staleness report", err)
	}
}

// A review of Config.Check, which ran gqlc -check and then a generate and
// compared. Everything below was found that way.

// The report, exactly: each path relative to Config.Dir once, the verbs in
// one order, the files sorted within each. Config.Dir given as a relative
// path is what gqlc -config gqlc.yaml passes, and stale SDL copies were then
// named with the output directory twice.
func TestCheckReportIsExact(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireFieldModule(t, scaffold, wireFieldDeps)
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Dir: filepath.Base(dir), SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold,
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Three to delete, found in an order that is not the report's: two
	// leftovers by the walk, and the SDL copy after them by the glob.
	writeFiles(t, dir, map[string]string{
		"graph/aaa/generated.go":   generatedHeader + "\n\npackage aaa\n",
		"graph/zzz/generated.go":   generatedHeader + "\n\npackage zzz\n",
		"graph/schema/old.graphql": "type Old { id: ID! }\n",
	})
	gen := filepath.Join(dir, "graph", "post", "generated.go")
	if err := os.WriteFile(gen, []byte(readFile(t, gen)+"\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "graph", "user", "generated.go")); err != nil {
		t.Fatal(err)
	}
	cfg.Check = true
	err := Generate(context.Background(), cfg)
	const want = "codegen: generated code is not current; a generate would\n" +
		"  create graph/user/generated.go\n" +
		"  rewrite graph/post/generated.go\n" +
		"  delete graph/aaa/generated.go\n" +
		"  delete graph/schema/old.graphql\n" +
		"  delete graph/zzz/generated.go"
	if err == nil || err.Error() != want {
		t.Fatalf("the report is\n%v\nwant\n%s", err, want)
	}
}

// A file that differs only in line endings is the same file: a checkout with
// core.autocrlf, the Windows default, turns every LF into CRLF, and both a
// generate and a check went by bytes, so every generated file was rewritten
// on every run and reported stale on every check.
func TestALineEndingIsNotAChange(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, generate := wireFieldModule(t, scaffold, wireFieldDeps)
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"graph/user/generated.go", "graph/schema/user.graphql"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(strings.ReplaceAll(readFile(t, p), "\n", "\r\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: scaffold, Wire: Wire{Deps: "hello/internal/deps.Deps"}, Check: true,
	})
	if err != nil {
		t.Errorf("CRLF line endings were reported as a change: %v", err)
	}
	before := readFile(t, filepath.Join(dir, "graph", "user", "generated.go"))
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, filepath.Join(dir, "graph", "user", "generated.go")); after != before {
		t.Error("a generate rewrote a file that differed only in line endings")
	}
}

// A check refuses what a generate refuses, each of them. They are checked at
// different points of a generate -- the configuration, the existing files,
// the group directories, the scaffold targets -- and a check that returned
// before any one of them called a broken setup current.
func TestCheckRefusesEachThingGenerateRefuses(t *testing.T) {
	both := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	for name, c := range map[string]struct {
		scaffold map[string]string
		files    map[string]string
		want     string
	}{
		"a scaffold key that is no group":        {map[string]string{"nosuch": "graph/x.Handler"}, nil, "nosuch"},
		"a scaffold target that is not dir.Type": {map[string]string{"user": "graph/user/Handler"}, nil, "dir.Type"},
		// A file at a path gqlc writes that gqlc did not write.
		"a hand-written file where gqlc would write": {both, map[string]string{"graph/user/generated.go": "package user\n"}, "generated.go"},
		// A group's package directory that is a file, which reading the
		// files there refuses first.
		"a group directory that is a file": {both, map[string]string{"graph/post": "not a directory\n"}, "post"},
		// One that is another module, which only the group-directory check
		// refuses.
		"a group directory that is another module": {both, map[string]string{"graph/post/go.mod": "module elsewhere\n"}, "post"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := wireFieldModule(t, both, wireFieldDeps)
			writeFiles(t, dir, c.files)
			cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph", Scaffold: c.scaffold}
			genErr := Generate(context.Background(), cfg)
			if genErr == nil {
				t.Fatal("the generate is not refused, so this case tests nothing")
			}
			if c.files == nil {
				// The generate wrote what it could before refusing; a check
				// compares against a tree as fresh as the generate's was.
				dir, _ = wireFieldModule(t, both, wireFieldDeps)
				cfg.Dir = dir
			}
			cfg.Check = true
			err := Generate(context.Background(), cfg)
			if err == nil || errors.Is(err, ErrStale) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("a generate says %v\nand a check says %v", genErr, err)
			}
		})
	}
}

// Output reached through a symlink is walked from where it really is, for
// the files to delete as for the generate's own prune.
func TestCheckFindsLeftoversThroughALinkedOutput(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, _ := wireFieldModule(t, scaffold, wireFieldDeps)
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "graph")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph", Scaffold: scaffold}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"real/old/generated.go": generatedHeader + "\n\npackage old\n"})
	cfg.Check = true
	if err := Generate(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "delete graph/old/generated.go") {
		t.Fatalf("err = %v, want the leftover found through the link and named under it", err)
	}
}
