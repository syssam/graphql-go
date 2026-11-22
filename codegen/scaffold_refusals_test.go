package codegen

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What scaffolding cannot do to a target is refused before the output is
// written, as every other refusal is, so a refused generate leaves the tree
// as it was and a check after it does not call the setup current:
//
//   - the file the stubs go into is a directory;
//   - it is excluded from every build, where the stubs, and the type with
//     them on each run, compiled nowhere;
//   - it does not parse, and no build includes it;
//   - two groups share a target and need one method with two signatures,
//     which no type can have.
func TestScaffoldRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	const one = "type Query { u: User }\ntype User { id: ID! }\n"
	for name, c := range map[string]struct {
		files    map[string]string
		scaffold map[string]string
		want     string
	}{
		"stub file is a directory": {
			files:    map[string]string{"schema/graph.graphql": one, "impl/graph.resolvers.go/keep": ""},
			scaffold: map[string]string{"graph": "impl.R"},
			want:     "graph.resolvers.go is a directory",
		},
		"stub file is in no build": {
			files:    map[string]string{"schema/graph.graphql": one, "impl/graph.resolvers.go": "//go:build ignore\n\npackage impl\n"},
			scaffold: map[string]string{"graph": "impl.R"},
			want:     "excluded from every build",
		},
		"stub file in no build does not parse": {
			files:    map[string]string{"schema/graph.graphql": one, "impl/graph.resolvers.go": "//go:build ignore\n\npackage impl\n\nfunc (\n"},
			scaffold: map[string]string{"graph": "impl.R"},
			want:     "excluded from every build",
		},
		"one method, two signatures": {
			files: map[string]string{
				"schema/alpha.graphql": "type A { n: Int! }\ntype Query { thing: A }\n",
				"schema/beta.graphql":  "type B { n: Int! }\ntype Mutation { thing(x: Int): B }\n",
			},
			scaffold: map[string]string{"alpha": "impl.Resolver", "beta": "impl.Resolver"},
			want:     "scaffold beta: Resolver needs Resolver.Thing(ctx context.Context, args betagql.ThingArgs)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for path, src := range c.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(path, "/keep") {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph", Scaffold: c.scaffold}
			before := snapshot(t, dir)
			err := Generate(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Generate: err = %v, want %q", err, c.want)
			}
			if after := snapshot(t, dir); !maps.Equal(before, after) {
				t.Errorf("a refused generate changed the tree")
			}
			if err := Check(context.Background(), cfg, CheckOptions{}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Check: err = %v, want the same refusal", err)
			}
		})
	}
}

// Two groups sharing a target where one method name has one signature in
// both is no conflict: one method implements both Resolvers.
func TestScaffoldSharedTargetWithOneSignatureIsAccepted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for path, src := range map[string]string{
		"schema/alpha.graphql": "type Query { count: Int }\n",
		"schema/beta.graphql":  "type Mutation { count: Int }\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Dir: dir, SchemaGlobs: []string{"schema/*.graphql"}, Output: "graph", Package: "hello/graph",
		Scaffold: map[string]string{"alpha": "impl.Resolver", "beta": "impl.Resolver"}}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}
