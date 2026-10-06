package codegen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every group scaffolded into one package -- a resolver layer, as
// examples/backoffice has -- used to reparse that whole package once per group:
// quadratic, and 41 s of a 300-entity generate that took 1.6 s with the same
// resolvers spread over thirty packages. A timing test would be flaky, so this
// counts parses: each file once per run, however many groups.
//
// A run reads the package twice, and the count is of both: before anything is
// written, for what scaffolding would refuse, and again to scaffold. Counted
// in the second alone, this passed while every file was parsed twice.
func TestScaffoldParsesEachFileOnce(t *testing.T) {
	const groups = 30
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
	scaffold := map[string]string{"query": "impl.QueryResolver"}
	write("schema/query.graphql", "type Query { ping: Boolean }\n")
	for i := range groups {
		write(fmt.Sprintf("schema/mod%02d.graphql", i),
			fmt.Sprintf("type M%02dT { id: ID! }\nextend type Query { q%02d: M%02dT }\n", i, i, i))
		scaffold[fmt.Sprintf("mod%02d", i)] = fmt.Sprintf("impl.M%02dResolver", i)
	}
	run := func() *builder {
		t.Helper()
		b, err := newBuilder(dir, Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "example.com/s/graph", Scaffold: scaffold,
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := b.emit()
		if err != nil {
			t.Fatal(err)
		}
		if err := b.checkScaffoldTargets(filepath.Join(dir, "graph"), out); err != nil {
			t.Fatal(err)
		}
		if err := b.scaffold(b.uniqueGroups()); err != nil {
			t.Fatal(err)
		}
		return b
	}
	run() // writes one file per group into impl/
	files, err := filepath.Glob(filepath.Join(dir, "impl", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != groups+1 {
		t.Fatalf("scaffolded %d files, want %d", len(files), groups+1)
	}
	before := map[string]string{}
	for _, f := range files {
		before[f] = readFile(t, f)
	}

	b := run() // every method exists now: nothing to write, only to read
	if b.scaffoldParses != len(files) {
		t.Errorf("scaffold parsed %d files for a package of %d; each must be parsed once per run", b.scaffoldParses, len(files))
	}
	// Handed over, not held: a parse kept for the whole run would answer a
	// package indexed once per group as well, and this count with it.
	if len(b.checked) != 0 {
		t.Errorf("%d parses from the check were still held after scaffolding took them", len(b.checked))
	}
	for _, f := range files {
		if readFile(t, f) != before[f] {
			t.Errorf("a run with nothing new changed %s", f)
		}
	}
}

// What the check parsed stands for a file only while the file is what it
// was. The run writes between the check and the scaffold, and a directory
// reached by a second path is written under one name and read under the
// other, so a parse is taken only for the bytes it was made from: taken by
// path alone, a method that arrived in between was scaffolded a second time.
func TestScaffoldParsesAgainAFileChangedSinceTheCheck(t *testing.T) {
	dir := t.TempDir()
	const impl = "package impl\n\ntype Resolver struct{}\n"
	writeFiles(t, dir, map[string]string{
		"schema/query.graphql": "type Query { ping: Boolean }\n",
		"schema/user.graphql":  "type User { id: ID! }\nextend type Query { user: User }\n",
		"impl/query.go":        impl,
	})
	b, err := newBuilder(dir, Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "example.com/s/graph",
		Scaffold: map[string]string{"query": "impl.Resolver", "user": "impl.Resolver"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.emit()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.checkScaffoldTargets(filepath.Join(dir, "graph"), out); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{
		"impl/query.go": impl + "\nfunc (r *Resolver) Ping() {}\n",
	})
	if err := b.scaffold(b.uniqueGroups()); err != nil {
		t.Fatal(err)
	}
	if b.scaffoldParses != 2 {
		t.Errorf("parsed %d times, want 2: once for the check and once for the file as it was by the scaffold", b.scaffoldParses)
	}
	if _, err := os.Stat(filepath.Join(dir, "impl", "query.resolvers.go")); err == nil {
		t.Errorf("Ping was scaffolded although the file had it by then:\n%s", readFile(t, filepath.Join(dir, "impl", "query.resolvers.go")))
	}
	if stubs := readFile(t, filepath.Join(dir, "impl", "user.resolvers.go")); !strings.Contains(stubs, ") User(") {
		t.Errorf("the method still missing was not scaffolded:\n%s", stubs)
	}
}

// What scaffolding would refuse is found before anything is written, by
// reading each target's package against the generated files that share its
// directory. Finding those by asking, for every target, whether each output
// file's directory is the target's was targets times files: a regenerate with
// nothing to do went from 0.4 s to 2.2 s at 600 groups. Counted, not timed.
func TestScaffoldTargetsAreMatchedToOutputInOnePass(t *testing.T) {
	const groups = 30
	dir := t.TempDir()
	files := map[string]string{"schema/query.graphql": "type Query { ping: Boolean }\n"}
	scaffold := map[string]string{"query": "graph/query.Handler"}
	for i := range groups {
		files[fmt.Sprintf("schema/mod%02d.graphql", i)] = fmt.Sprintf("type M%02dT { id: ID! }\nextend type Query { q%02d: M%02dT }\n", i, i, i)
		// Half beside the generated code and half in a package of their own,
		// which no output directory matches however it is asked.
		scaffold[fmt.Sprintf("mod%02d", i)] = fmt.Sprintf("graph/mod%02d.Handler", i)
		if i%2 == 1 {
			scaffold[fmt.Sprintf("mod%02d", i)] = fmt.Sprintf("impl/mod%02d.Handler", i)
		}
	}
	writeFiles(t, dir, files)
	cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "example.com/s/graph", Scaffold: scaffold}
	// Once for real, so that the second pass has directories to ask about.
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	b, err := newBuilder(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.emit()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.checkScaffoldTargets(filepath.Join(dir, "graph"), out); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for rel := range out {
		dirs[filepath.Dir(rel)] = true
	}
	// Each output directory and each target is looked at a fixed number of
	// times; a target per output file is forty times this.
	if limit := 2 * (len(dirs) + len(scaffold)); b.scaffoldProbes > limit {
		t.Errorf("checkScaffoldTargets asked the filesystem %d times for %d output directories and %d targets, want at most %d",
			b.scaffoldProbes, len(dirs), len(scaffold), limit)
	}
	// And it still finds the generated names in a target's own directory.
	scaffold["mod00"] = "graph/mod00.Resolver"
	if err := b.checkScaffoldTargets(filepath.Join(dir, "graph"), out); err == nil || !strings.Contains(err.Error(), "Resolver is declared by the generated code") {
		t.Errorf("a target type the generated code declares was not refused: %v", err)
	}
}

// Two groups implemented by one type, as a single root Resolver does: the
// second group's stubs must see the type the first one just declared, or the
// package declares it twice. Reading the directory once per run is what made
// that something to keep true on purpose.
func TestScaffoldTwoGroupsIntoOneType(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go vet subprocess")
	}
	dir, _ := scaffoldModule(t, nil)
	var notes []string
	generate := func() {
		t.Helper()
		if err := Generate(context.Background(), Config{
			Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
			Output: "graph", Package: "hello/graph",
			Scaffold: map[string]string{"user": "impl.Resolver", "post": "impl.Resolver"},
			Notef:    func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) },
		}); err != nil {
			t.Fatal(err)
		}
	}
	generate()
	generate()
	// Neither group's methods are stale for the other: the type implements
	// both, and telling the author to delete them would break the build.
	for _, n := range notes {
		if strings.Contains(n, "has methods") {
			t.Errorf("a method another group asks for was called stale: %s", n)
		}
	}
	goBuild(t, dir)
}
