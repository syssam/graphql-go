package codegen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refusal about a place in the SDL says where, as file:line:column
// relative to Config.Dir, and Diagnostics finds each one in a joined error.
func TestRefusalsCarryTheirSDLPosition(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		sdl          string
		cfg          func(*Config)
		file         string
		line, column int
		coord        string
	}{
		"name collision": {
			sdl:  "type Query {\n  node: Int\n}\ntype Mutation {\n  node: Int\n}\n",
			file: "schema/s.graphql",
			line: 5, column: 3,
			coord: "Mutation.node",
		},
		"list depth": {
			sdl:  "type Query {\n  grid: [[[Int]]]\n}\n",
			file: "schema/s.graphql",
			line: 2, column: 3,
			coord: "Query.grid",
		},
		"unread directive argument": {
			sdl:  "directive @goField(forceResolver: Boolean, name: String) on FIELD_DEFINITION\ntype Query {\n  a: Int @goField(name: \"B\")\n}\n",
			cfg:  func(c *Config) { c.FieldDirective = FieldDirective{Name: "goField", ForceResolverArg: "forceResolver"} },
			file: "schema/s.graphql",
			line: 3, column: 19,
			coord: "Query.a",
		},
		"tag directive": {
			sdl:  "directive @goTag(key: String!, value: String) repeatable on INPUT_FIELD_DEFINITION\ninput In {\n  a: Int @goTag(key: \"graphql\", value: \"x\")\n}\ntype Query { q(i: In): Int }\n",
			cfg:  func(c *Config) { c.TagDirective = goTag },
			file: "schema/s.graphql",
			line: 3, column: 3,
			coord: "In.a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "schema", "s.graphql"), []byte(c.sdl), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph"}
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			_, err := Render(context.Background(), cfg)
			ds := Diagnostics(err)
			if len(ds) != 1 {
				t.Fatalf("Diagnostics = %v, want one, from %v", ds, err)
			}
			d := ds[0]
			if d.File != c.file || d.Line != c.line || d.Column != c.column || d.Coord != c.coord {
				t.Fatalf("got %s:%d:%d %s, want %s:%d:%d %s", d.File, d.Line, d.Column, d.Coord, c.file, c.line, c.column, c.coord)
			}
			if want := fmt.Sprintf("%s:%d:%d: ", c.file, c.line, c.column); !strings.HasPrefix(err.Error(), want) {
				t.Errorf("err = %q, want it to start %q", err, want)
			}
		})
	}
}

// Diagnostics walks errors.Join and %w, in order, and skips what is not one.
func TestDiagnosticsWalksTheWholeTree(t *testing.T) {
	t.Parallel()
	a := &Diagnostic{Coord: "A", Message: "a"}
	b := &Diagnostic{File: "s.graphql", Line: 1, Column: 2, Coord: "B", Message: "b"}
	err := errors.Join(a, errors.New("plain"), fmt.Errorf("wrapped: %w", errors.Join(b)))
	ds := Diagnostics(err)
	if len(ds) != 2 || ds[0] != a || ds[1] != b {
		t.Fatalf("Diagnostics = %v, want [a b]", ds)
	}
	if a.Error() != "a" || b.Error() != "s.graphql:1:2: b" {
		t.Errorf("Error() = %q, %q", a.Error(), b.Error())
	}
	if Diagnostics(nil) != nil || Diagnostics(errors.New("x")) != nil {
		t.Error("Diagnostics found something in an error with none")
	}
}

// Two root fields that collide are told the fix that changes nothing a
// client sees, and that fix works as written.
func TestRootFieldCollisionSaysWhichFieldNamesEntryFixesIt(t *testing.T) {
	t.Parallel()
	sdl := "type Query { node(id: ID): Int }\ntype Mutation { node(id: ID): Int }\n"
	_, err := generateWith(t, sdl, Config{})
	const want = `Config.FieldNames["Mutation.node"] = "MutationNode" (gqlc.yaml: fieldNames: {Mutation.node: MutationNode})`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v\nwant it to contain %s", err, want)
	}
	if _, err := generateWith(t, sdl, Config{FieldNames: map[string]string{"Mutation.node": "MutationNode"}}); err != nil {
		t.Fatalf("the suggested entry does not fix it: %v", err)
	}
	// Two object fields keep the advice that fits them.
	_, err = generateWith(t, "type Query { a: A b: B }\ntype A { bC: Int }\ntype AB { c: Int }\ntype B { x: Int }\n", Config{
		FieldNames: map[string]string{"A.bC": "BC"},
	})
	if err != nil && strings.Contains(err.Error(), "FieldNames[") {
		t.Errorf("a non-root collision was given the root-field advice: %v", err)
	}
}
