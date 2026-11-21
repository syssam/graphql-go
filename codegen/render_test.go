package codegen

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const renderSDL = "type Query { user(id: ID!): User }\ntype User { id: ID! name: String }\n"

func renderConfig(t *testing.T, sdl string) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{Dir: dir, SchemaGlobs: []string{"schema.graphql"}, Output: "graph", Package: "hello/graph"}
}

// Render returns what a generate writes, byte for byte and file for file,
// and writes nothing itself.
func TestRenderIsWhatAGenerateWritesAndWritesNothing(t *testing.T) {
	cfg := renderConfig(t, renderSDL)
	before := snapshot(t, cfg.Dir)
	files, err := Render(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, cfg.Dir); !maps.Equal(before, after) {
		t.Fatalf("Render changed the tree: %v", slices.Sorted(maps.Keys(after)))
	}
	if err := Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	written := map[string]string{}
	for rel, src := range snapshot(t, cfg.Dir) {
		if out, ok := strings.CutPrefix(rel, "graph/"); ok {
			written[out] = src
		}
	}
	rendered := map[string]string{}
	for rel, src := range files {
		rendered[rel] = string(src)
	}
	if !maps.Equal(rendered, written) {
		t.Fatalf("Render returned %v\na generate wrote %v", slices.Sorted(maps.Keys(rendered)), slices.Sorted(maps.Keys(written)))
	}
}

// Render refuses what a generate refuses about the schema and the
// configuration: a caller previewing the output must not be shown files a
// generate would never write.
func TestRenderRefusesWhatAGenerateRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		sdl  string
		cfg  func(*Config)
		want string
	}{
		"name collision": {
			sdl:  "type Query { node: Int }\ntype Mutation { node: Int }\n",
			want: "both generate",
		},
		"inline names no field": {
			sdl:  renderSDL,
			cfg:  func(c *Config) { c.Inline = []string{"User.nope"} },
			want: `inline "User.nope" names no field`,
		},
		"scaffold names no group": {
			sdl:  renderSDL,
			cfg:  func(c *Config) { c.Scaffold = map[string]string{"nogroup": "graph/x.Handler"} },
			want: "nogroup",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := renderConfig(t, c.sdl)
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			files, err := Render(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if files != nil {
				t.Errorf("a refused Render returned %d files", len(files))
			}
		})
	}
}

// A cancelled context stops Render, Check and Generate before they read the
// schema, and nothing is written.
func TestCancelledContextIsRefusedFirst(t *testing.T) {
	cfg := renderConfig(t, renderSDL)
	before := snapshot(t, cfg.Dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Render(ctx, cfg); err != context.Canceled {
		t.Errorf("Render: err = %v, want context.Canceled", err)
	}
	if err := Check(ctx, cfg, CheckOptions{}); err != context.Canceled {
		t.Errorf("Check: err = %v, want context.Canceled", err)
	}
	if err := Generate(ctx, cfg); err != context.Canceled {
		t.Errorf("Generate: err = %v, want context.Canceled", err)
	}
	if after := snapshot(t, cfg.Dir); !maps.Equal(before, after) {
		t.Errorf("a cancelled run changed the tree")
	}
}
