package codegen

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The generated package embeds every schema/*.graphql it holds, so an SDL
// file removed from the sources must leave the output too, or its types stay
// in the served schema.
func TestDeletedSDLLeavesTheEmbeddedSchema(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, "sdl", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("query.graphql", "type Query { ping: Boolean }\n")
	write("legacy.graphql", "type Legacy { id: ID! }\nextend type Query { legacy: Legacy }\n")
	gen := func() {
		t.Helper()
		if err := Generate(context.Background(), Config{Dir: dir, SchemaGlobs: []string{"sdl/*.graphql"}, Output: "graph", Package: "hello/graph"}); err != nil {
			t.Fatal(err)
		}
	}
	gen()
	copyOf := filepath.Join(dir, "graph", "schema", "legacy.graphql")
	if _, err := os.Stat(copyOf); err != nil {
		t.Fatalf("the first run should embed legacy.graphql: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "sdl", "legacy.graphql")); err != nil {
		t.Fatal(err)
	}
	gen()
	if _, err := os.Stat(copyOf); !os.IsNotExist(err) {
		t.Errorf("legacy.graphql was deleted from the sources but is still embedded (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "schema", "query.graphql")); err != nil {
		t.Errorf("a file still in the sources was removed: %v", err)
	}
}
