package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesBindings(t *testing.T) {
	dir := t.TempDir()
	sdl := "type User { id: ID! name: String! }\ntype Query { users: [User!]! }\n"
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := "schema: [schema.graphql]\noutput: graph\npackage: example/graph\n"
	cfg := filepath.Join(dir, "gqlc.yaml")
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "generated.go")); err != nil {
		t.Fatal(err)
	}
}

// -stubs fails a check; on a generate, which has just written the stubs it
// would fail over, it is refused rather than ignored.
func TestStubsFlagNeedsCheck(t *testing.T) {
	err := run([]string{"-stubs", "-schema", "x.graphql", "-out", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "pass -check with it") {
		t.Fatalf("err = %v, want -stubs refused without -check", err)
	}
}
