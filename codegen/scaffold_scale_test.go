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
// examples/veloxfx has -- used to reparse that whole package once per group:
// quadratic, and 41 s of a 300-entity generate that took 1.6 s with the same
// resolvers spread over thirty packages. A timing test would be flaky, so this
// counts parses: each file at most once per run, however many groups.
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
		if _, err := b.emit(); err != nil {
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
	if b.scaffoldParses > len(files) {
		t.Errorf("scaffold parsed %d files for a package of %d; each must be parsed once per run", b.scaffoldParses, len(files))
	}
	for _, f := range files {
		if readFile(t, f) != before[f] {
			t.Errorf("a run with nothing new changed %s", f)
		}
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
