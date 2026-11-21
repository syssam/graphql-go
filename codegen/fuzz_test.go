package codegen

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzRender feeds arbitrary SDL to Render. Whatever the schema, gqlc either
// refuses it or produces Go: it never panics, never fails formatting its own
// output -- which is gqlc writing Go that does not parse, from text it copied
// out of the SDL without escaping -- and produces the same files every time.
func FuzzRender(f *testing.F) {
	for _, seed := range []string{
		"type Query { a: Int }",
		`"""doc */ ` + "`" + ` \" """ type Query { "x" a("y" b: String = "q\""): Int }`,
		"enum E { A B _C } input In { e: E = A l: [[In]] } type Query { q(i: In = {e: B}): E }",
		"interface Node { id: ID! } type A implements Node { id: ID! } union U = A type Query { n: Node u: U }",
		"type Query { _x: Int x: Int } type Subscription { s: Int }",
		"scalar Time type Query { t(a: Time = \"2020\"): Time }",
		"type Query { type: Int func: Int range(map: Int): [[String!]] }",
		"\"\"\"\n// line\n/* block\n\"\"\"\nenum Status { ACTIVE \"\"\"*/\"\"\" INACTIVE }\ntype Query { s: Status }",
		"type Query { node: Int } type Mutation { node: Int }",
		"type Query {\n  \"\"\"\n  first line\n  second line\n  \"\"\"\n  a(x: Int): Int\n}",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, sdl string) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph"}
		files, err := Render(context.Background(), cfg)
		if err != nil {
			if strings.Contains(err.Error(), "codegen: format ") {
				t.Fatalf("gqlc produced Go that does not parse, for SDL %q:\n%v", sdl, err)
			}
			return
		}
		again, err := Render(context.Background(), cfg)
		if err != nil {
			t.Fatalf("a second Render refused what the first accepted: %v", err)
		}
		if len(again) != len(files) {
			t.Fatalf("Render produced %d files, then %d", len(files), len(again))
		}
		for rel, src := range files {
			if !bytes.Equal(again[rel], src) {
				t.Fatalf("Render produced %s differently twice", rel)
			}
		}
	})
}
