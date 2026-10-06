package codegen

import (
	"context"
	"errors"
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

// A source is copied under its own stem and .graphql, whatever it is called:
// the root package embeds schema/*.graphql, and gqlgen's schema.graphqls, kept
// under that name, was a copy nothing embedded. With every source named so
// the package did not compile; with some, it compiled and the schema was
// missing their types.
func TestSchemaSourcesOfAnyExtensionAreEmbedded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go test subprocess")
	}
	dir := t.TempDir()
	const query, user, post = "type Query { ping: Boolean }\n",
		"type User { id: ID! name: String! }\nextend type Query { user(id: ID!): User }\n",
		"type Post { id: ID! title: String! }\nextend type Query { post(id: ID!): Post }\n"
	writeFiles(t, dir, map[string]string{
		"gqlgen/query.graphqls": query, "gqlgen/user.graphqls": user, "gqlgen/post.graphqls": post,
		"mixed/query.graphql": query, "mixed/user.graphqls": user, "mixed/post.gql": post,
		"plain/query.graphql": query, "plain/user.graphql": user, "plain/post.graphql": post,
	})
	var roots []string
	for _, name := range []string{"gqlgen", "mixed", "plain"} {
		cfg := Config{Dir: dir, SchemaGlobs: []string{name + "/*"}, Output: name + "/graph", Package: "hello/" + name + "/graph"}
		if err := Generate(context.Background(), cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, stem := range []string{"query", "user", "post"} {
			if _, err := os.Stat(filepath.Join(dir, name, "graph", "schema", stem+".graphql")); err != nil {
				t.Errorf("%s: the copy of %s is not where the root package embeds it from: %v", name, stem, err)
			}
		}
		cfg.Check = true
		if err := Generate(context.Background(), cfg); err != nil {
			t.Errorf("%s: a check straight after a generate: %v", name, err)
		}
		roots = append(roots, "hello/"+name+"/graph")
	}
	validateGenerated(t, dir, roots...)
}

// Two sources whose copies would have one name are refused, and a name is
// compared without letter case and without its extension: on a filesystem
// that ignores case User.graphql and user.graphql are one file, which two
// writes at once left holding parts of both.
func TestGenerateRefusesSourcesWhoseCopiesCollide(t *testing.T) {
	for name, files := range map[string][2]string{
		"letter case": {"a/User.graphql", "b/user.graphql"},
		"extension":   {"a/user.graphqls", "b/user.graphql"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{
				files[0]: "type User { id: ID! }\ntype Query { me: User }\n",
				files[1]: "type Post { id: ID! }\nextend type Query { post: Post }\n",
			})
			err := Generate(context.Background(), Config{
				Dir: dir, SchemaGlobs: []string{"a/*", "b/*"}, Output: "graph", Package: "hello/graph",
			})
			if err == nil {
				t.Fatal("generated from two sources with one copy between them")
			}
			for _, want := range files {
				if !strings.Contains(filepath.ToSlash(err.Error()), want) {
					t.Errorf("the error does not name %s: %v", want, err)
				}
			}
			if _, serr := os.Stat(filepath.Join(dir, "graph")); serr == nil {
				t.Error("output was written before the collision was reported")
			}
		})
	}
}

// gqlc deletes a copy only when it wrote it, which the list in schema.go
// says: a file in Output/schema that is neither a source's copy nor on that
// list is somebody's SDL, and it was deleted because nothing told the two
// apart. It cannot be left either, since the root embeds the directory, so it
// is named and the generate refused, by a check as well.
func TestGenerateNeverDeletesAnSDLFileItDidNotWrite(t *testing.T) {
	const legacy = "# kept by hand\ntype Legacy { id: ID! }\n"
	refused := func(t *testing.T, cfg Config, dir string) {
		t.Helper()
		before := snapshot(t, dir)
		for _, check := range []bool{false, true} {
			cfg.Check = check
			err := Generate(context.Background(), cfg)
			if err == nil || errors.Is(err, ErrStale) || !strings.Contains(filepath.ToSlash(err.Error()), "graph/schema/legacy.graphql") {
				t.Errorf("check=%v: err = %v, want a refusal naming graph/schema/legacy.graphql", check, err)
			}
		}
		after := snapshot(t, dir)
		if after["graph/schema/legacy.graphql"] != legacy {
			t.Errorf("legacy.graphql is gone or changed: %q", after["graph/schema/legacy.graphql"])
		}
		if len(after) != len(before) {
			t.Errorf("a refused generate wrote or deleted files: %d before, %d after", len(before), len(after))
		}
	}
	t.Run("before the first generate", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"api/query.graphql": "type Query { ok: Boolean! }\n", "graph/schema/legacy.graphql": legacy})
		refused(t, Config{Dir: dir, SchemaGlobs: []string{"api/*.graphql"}, Output: "graph", Package: "hello/graph"}, dir)
	})
	t.Run("beside copies gqlc wrote", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"api/query.graphql": "type Query { ok: Boolean! }\n", "api/b.graphql": "type B { id: ID! }\n"})
		cfg := Config{Dir: dir, SchemaGlobs: []string{"api/*.graphql"}, Output: "graph", Package: "hello/graph"}
		if err := Generate(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, dir, map[string]string{"graph/schema/legacy.graphql": legacy})
		if err := os.Remove(filepath.Join(dir, "api", "b.graphql")); err != nil {
			t.Fatal(err)
		}
		refused(t, cfg, dir)
		// With it out of the way, the copy gqlc did write goes as before.
		if err := os.Remove(filepath.Join(dir, "graph", "schema", "legacy.graphql")); err != nil {
			t.Fatal(err)
		}
		if err := Generate(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "b.graphql")); !os.IsNotExist(err) {
			t.Errorf("the copy of a deleted source, which gqlc wrote, is still embedded (stat err %v)", err)
		}
	})
	// Output from before schema.go carried the list says nothing about what
	// gqlc wrote, so nothing there is known to be its own.
	t.Run("output with no list", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"api/query.graphql": "type Query { ok: Boolean! }\n", "api/legacy.graphql": legacy})
		cfg := Config{Dir: dir, SchemaGlobs: []string{"api/*.graphql"}, Output: "graph", Package: "hello/graph"}
		if err := Generate(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		schema := filepath.Join(dir, "graph", "schema.go")
		src := readFile(t, schema)
		start, end := strings.Index(src, "// sdl is"), strings.Index(src, "//go:embed")
		if start < 0 || end < start {
			t.Fatalf("schema.go carries no list of the copies:\n%s", src)
		}
		if err := os.WriteFile(schema, []byte(src[:start]+src[end:]), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "api", "legacy.graphql")); err != nil {
			t.Fatal(err)
		}
		refused(t, cfg, dir)
	})
}

// The list of copies is written by one function and read back by another,
// through gofmt and whatever a checkout does to line endings in between.
func TestRecordedCopiesReadsBackWhatWasWritten(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"api/query.graphql":      "type Query { ok: Boolean! }\n",
		"api/user.graphqls":      "type User { id: ID! }\n",
		"api/order item.graphql": "type OrderItem { id: ID! }\n",
	})
	if err := Generate(context.Background(), Config{Dir: dir, SchemaGlobs: []string{"api/*"}, Output: "graph", Package: "hello/graph"}); err != nil {
		t.Fatal(err)
	}
	src := readFile(t, filepath.Join(dir, "graph", "schema.go"))
	const want = "order item.graphql query.graphql user.graphql"
	for name, schemaGo := range map[string]string{
		"as written":               src,
		"with CRLF":                strings.ReplaceAll(src, "\n", "\r\n"),
		"behind a byte-order mark": "\ufeff" + src,
	} {
		copies, _ := recordedIn([]byte(schemaGo))
		if got := strings.Join(copies, " "); got != want {
			t.Errorf("%s: read back %q, want %q", name, got, want)
		}
	}
	for name, schemaGo := range map[string]string{
		"no file":                    "",
		"does not parse":             "package graph\n\nfunc (\n",
		"no list":                    "package graph\n\nimport \"embed\"\n\n//go:embed schema/*.graphql\nvar sdl embed.FS\n",
		"another variable's comment": "package graph\n\n// other lists:\n//\n//\tx.graphql\nvar other int\n",
	} {
		if got, _ := recordedIn([]byte(schemaGo)); got != nil {
			t.Errorf("%s: read back %q from a file that lists nothing", name, got)
		}
	}
}
