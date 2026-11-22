package codegen

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A model package renamed to make way for a mapped one does not take a name
// that is itself taken. Group time's models yield to the mapped time.Time,
// and the name they yield to, timemodel, is another group's model package, or
// the qualifier of a second mapped package as well; each was a redeclaration
// in a DO NOT EDIT file.
func TestRenamedModelPackageTakesAFreeName(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping compile")
	}
	for name, c := range map[string]struct {
		files  map[string]string
		models map[string]string
	}{
		"another group's package": {
			files: map[string]string{
				"schema/time.graphql":      "scalar Time\ntype B { at: Time! }\ntype Query { bs: [B!]! }\n",
				"schema/timemodel.graphql": "type A { n: Int! }\nextend type Query { as: [A!]! both: B }\n",
			},
			models: map[string]string{"Time": "time.Time"},
		},
		"and a mapped package's qualifier": {
			files: map[string]string{
				"schema/time.graphql":      "scalar Time\nscalar Stamp\ntype B { at: Time! s: Stamp! }\ntype Query { bs: [B!]! }\n",
				"schema/timemodel.graphql": "type A { n: Int! s: Stamp! }\nextend type Query { as: [A!]! both: B }\n",
				"x/timemodel/stamp.go":     "package timemodel\n\ntype Stamp string\n",
			},
			models: map[string]string{"Time": "time.Time", "Stamp": "hello/x/timemodel.Stamp"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTempModule(t, dir)
			for path, src := range c.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph", Models: c.models}
			if err := Generate(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "graph", "model", "timemodel")); err != nil {
				t.Fatalf("models are not split by group, so this does not test the collision: %v", err)
			}
			goBuild(t, dir)
		})
	}
}
