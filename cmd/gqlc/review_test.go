package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The module line is read with its grammar: a trailing comment ends it, the
// path may be quoted, and a word that only begins with "module" is not the
// keyword. Read as the text after "module", the comment became part of the
// import path every generated file wrote.
func TestModulePathReadsTheLineAsGoDoes(t *testing.T) {
	t.Parallel()
	for gomod, want := range map[string]string{
		"module example.com/app // the app\n":                "example.com/app",
		"module example.com/app//no space\n":                 "example.com/app",
		"module \"example.com/quoted\"\n":                    "example.com/quoted",
		"module `example.com/raw`\n":                         "example.com/raw",
		"// module example.com/commented\nmodule real.com\n": "real.com",
		"modules example.com/nope\nmodule example.com/yes\n": "example.com/yes",
	} {
		path := filepath.Join(t.TempDir(), "go.mod")
		if err := os.WriteFile(path, []byte(gomod+"\ngo 1.27\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := modulePath(path); err != nil || got != want {
			t.Errorf("modulePath(%q) = %q, %v; want %q", gomod, got, err, want)
		}
	}
}

// And the flag form writes that path into the generated imports.
func TestFlagFormIgnoresTheGoModComment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app // the app\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(flagSDL), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	if err := run([]string{"-schema", "schema.graphql", "-out", "graph"}); err != nil {
		t.Fatal(err)
	}
	if src := generated(t, dir, "graph/generated.go"); !strings.Contains(src, `"example.com/app/graph/model"`) {
		t.Fatalf("generated.go does not import example.com/app/graph/model:\n%s", src)
	}
}

// A second YAML document is refused, not ignored: KnownFields saw none of its
// keys, so a misspelling there was silent.
func TestASecondYAMLDocumentIsRefused(t *testing.T) {
	t.Parallel()
	_, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n---\nnullableInputOmitable: true\n",
		"type Query { a: String }\n")
	err := run([]string{"-config", cfg})
	if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("err = %v, want the second document refused", err)
	}
	// So is one behind an empty one.
	_, cfg = write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n---\n---\nnullableInputOmitable: true\n",
		"type Query { a: String }\n")
	if err := run([]string{"-config", cfg}); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("err = %v, want the third document refused", err)
	}
	// A trailing separator with nothing after it, or a comment, is one document.
	for _, tail := range []string{"---\n", "---\n# done\n"} {
		_, cfg = write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n"+tail, "type Query { a: String }\n")
		if err := run([]string{"-config", cfg}); err != nil {
			t.Fatalf("a trailing %q was refused: %v", tail, err)
		}
	}
}

// A stale check exits 1 and anything else that fails exits 2, as diff does,
// so CI can tell "regenerate" from "the configuration is broken".
func TestExitCodeTellsStaleFromBroken(t *testing.T) {
	t.Parallel()
	dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n", "type Query { a: String }\n")
	stale := run([]string{"-config", cfg, "-check"})
	if got := exitCode(stale); got != 1 {
		t.Errorf("a stale check (%v) exits %d, want 1", stale, got)
	}
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	if got := exitCode(run([]string{"-config", cfg, "-check"})); got != 0 {
		t.Errorf("a current check exits %d, want 0", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte("type Query { a: Nope }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := run([]string{"-config", cfg, "-check"})
	if got := exitCode(broken); got != 2 {
		t.Errorf("a broken schema (%v) exits %d, want 2", broken, got)
	}
	// And a stub left, with -stubs, is stale too.
	dir, cfg = write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\nscaffold:\n  graph: impl.Resolver\n", "type Query { a: String }\n")
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	stubs := run([]string{"-config", cfg, "-check", "-stubs"})
	if got := exitCode(stubs); got != 1 {
		t.Errorf("a stub left (%v) exits %d, want 1", stubs, got)
	}
	// And so is a method left behind by a dropped field, with -orphans.
	if err := os.WriteFile(filepath.Join(dir, "impl", "extra.go"), []byte("package impl\n\nfunc (r *Resolver) Gone() bool { return false }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphans := run([]string{"-config", cfg, "-check", "-orphans"})
	if got := exitCode(orphans); got != 1 || !strings.Contains(orphans.Error(), "Gone") {
		t.Errorf("an orphan left (%v) exits %d, want 1 naming Gone", orphans, got)
	}
	if got := exitCode(errors.New("x")); got != 2 {
		t.Errorf("an unrelated error exits %d, want 2", got)
	}
}
