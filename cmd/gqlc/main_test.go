package main

import (
	"os"
	"path/filepath"
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
