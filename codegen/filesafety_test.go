package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The SDL copies the root package embeds were keyed by base name, so a
// schema.graphql per feature folder left one copy standing for both: the
// generate succeeded, the output compiled, and the service failed at start-up
// on the types of the file that was dropped.
func TestGenerateRefusesSourcesThatShareABaseName(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"user/schema.graphql": `type User { id: ID! } type Query { me: User }`,
		"post/schema.graphql": `type Post { id: ID! } extend type Query { post: Post }`,
	})
	err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"user/schema.graphql", "post/schema.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	})
	if err == nil {
		t.Fatal("generated from two sources named schema.graphql, of which only one copy can be embedded")
	}
	for _, want := range []string{"user/schema.graphql", "post/schema.graphql", "schema.graphql"} {
		if !strings.Contains(filepath.ToSlash(err.Error()), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "graph")); serr == nil {
		t.Error("output was written before the collision was reported")
	}
}

// Go files gqlc writes carry its header, and prune reads that header before
// deleting anything. Writing did not: a hand-written file at a path gqlc
// generates was replaced without a word.
func TestGenerateRefusesToOverwriteAHandWrittenGoFile(t *testing.T) {
	const mine = "package graph\n\n// Mine.\nfunc Mine() {}\n"
	for _, rel := range []string{"graph/generated.go", "graph/schema.go", "graph/model/models.go"} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"schema.graphql": helloSDL, rel: mine})
			err := Generate(context.Background(), Config{
				Dir:         dir,
				SchemaGlobs: []string{"schema.graphql"},
				Output:      "graph",
				Package:     "hello/graph",
			})
			if err == nil {
				t.Fatal("generate succeeded over a file it did not write")
			}
			if !strings.Contains(filepath.ToSlash(err.Error()), rel) {
				t.Errorf("error does not name %s: %v", rel, err)
			}
			got, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if rerr != nil || string(got) != mine {
				t.Fatalf("the hand-written file did not survive: %q %v", got, rerr)
			}
			// Nothing else was written either: a refusal part-way through
			// would leave a package that is half this run's.
			for _, other := range []string{"graph/generated.go", "graph/schema.go", "graph/model/models.go"} {
				if other == rel {
					continue
				}
				if _, serr := os.Stat(filepath.Join(dir, filepath.FromSlash(other))); serr == nil {
					t.Errorf("%s was written although the generate was refused", other)
				}
			}
		})
	}
}

// Regenerating over gqlc's own output is the ordinary case and must stay one.
func TestGenerateOverwritesItsOwnOutput(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"schema.graphql": helloSDL})
	cfg := Config{Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph"}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"schema.graphql": helloSDL + "\ntype Extra { id: ID! }\n"})
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatalf("second generate over gqlc's own files: %v", err)
	}
	models, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil || !strings.Contains(string(models), "Extra") {
		t.Fatalf("models.go was not regenerated: %v", err)
	}
}

// gqlc owns Output/schema and deletes copies there that this run did not
// write, since the root package embeds the directory by glob. When the
// sources themselves live in that directory, a file the globs do not name is
// the author's own, and it was deleted all the same.
func TestGenerateDoesNotDeleteAnSDLFileBesideItsSources(t *testing.T) {
	dir := t.TempDir()
	const draft = "type Draft { id: ID! }\n"
	writeFiles(t, dir, map[string]string{
		"graph/schema/api.graphql":   `type Query { ok: Boolean! }`,
		"graph/schema/draft.graphql": draft,
	})
	err := Generate(context.Background(), Config{
		Dir:         dir,
		SchemaGlobs: []string{"graph/schema/api.graphql"},
		Output:      "graph",
		Package:     "hello/graph",
	})
	got, rerr := os.ReadFile(filepath.Join(dir, "graph", "schema", "draft.graphql"))
	if rerr != nil || string(got) != draft {
		t.Fatalf("draft.graphql was deleted or changed (generate err: %v): %q %v", err, got, rerr)
	}
	// It cannot be left silently either: the root embeds every file in that
	// directory, so the draft's types would be served.
	if err == nil || !strings.Contains(filepath.ToSlash(err.Error()), "graph/schema/draft.graphql") {
		t.Fatalf("err = %v, want one naming graph/schema/draft.graphql", err)
	}
}

// The case prune exists for is unchanged: a copy whose source went away.
func TestGenerateStillPrunesAStaleSchemaCopy(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.graphql": `type Query { ok: Boolean! }`,
		"b.graphql": `type B { id: ID! }`,
	})
	cfg := Config{Dir: dir, SchemaGlobs: []string{"*.graphql"}, Output: "graph", Package: "hello/graph"}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "b.graphql")); err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "b.graphql")); err == nil {
		t.Fatal("the copy of a deleted source is still embedded")
	}
}

// An absolute Output was joined under Dir like a relative one, so
// /srv/app/graph became <dir>/srv/app/graph.
func TestGenerateHonoursAnAbsoluteOutput(t *testing.T) {
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "graph")
	writeFiles(t, dir, map[string]string{"schema.graphql": helloSDL})
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: out, Package: "hello/graph",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "generated.go")); err != nil {
		t.Fatalf("nothing was generated at the absolute output: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the output was also written under Dir: %v", entries)
	}
}

// A generate that is interrupted can leave a file created and not yet written.
// It carries no header, and refusing it as somebody else's would fail every
// generate after until it was deleted by hand.
func TestGenerateOverwritesAnEmptyGoFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"schema.graphql": helloSDL, "graph/generated.go": "", "graph/schema.go": "\n"})
	err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph",
	})
	if err != nil {
		t.Fatalf("an empty file at a generated path blocked the generate: %v", err)
	}
	if src, _ := os.ReadFile(filepath.Join(dir, "graph", "generated.go")); !isGeneratedSource(src) {
		t.Fatal("the empty file was not replaced by generated output")
	}
}
