package codegen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A federated schema split across files -- the shape a generator produces --
// generates, compiles, and builds as a subgraph: @key needs no declaration,
// NewSchema takes the entity resolvers, and ValidateSchema builds with a
// placeholder for each @key type.
func TestFederationGeneratesASubgraph(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go run subprocess")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"schema/user.graphql":   "type User @key(fields: \"id\") { id: ID! name: String! }\nextend type Query { me: User }\n",
		"schema/post.graphql":   "type Post @key(fields: \"id\") @shareable { id: ID! title: String! author: User! }\nextend type Query { post(id: ID!): Post }\n",
		"schema/schema.graphql": "type Query { ping: Boolean }\n",
		"check/main.go":         "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n\t\"hello/graph\"\n)\n\nfunc main() {\n\tif err := graph.ValidateSchema(); err != nil {\n\t\tfmt.Println(err)\n\t\tos.Exit(1)\n\t}\n}\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTempModule(t, dir)
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "hello/graph", Federation: true,
	}); err != nil {
		t.Fatal(err)
	}
	src := readFile(t, filepath.Join(dir, "graph", "schema.go"))
	for _, want := range []string{
		"func NewSchema(entities []fed.Entity, opts ...graphql.SchemaOption)",
		`fed.Resolver("Post", none)`, `fed.Resolver("User", none)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("schema.go lacks %q:\n%s", want, src)
		}
	}
	goBuild(t, dir)
	run := exec.Command("go", "run", "./check")
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("ValidateSchema on the generated subgraph: %v\n%s", err, out)
	}
}
