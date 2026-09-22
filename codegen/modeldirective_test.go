package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSchema(t *testing.T, sdl string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema", "s.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const goModelSDL = `
directive @goModel(model: String, models: [String!], forceGenerate: Boolean) on OBJECT | INPUT_OBJECT | SCALAR | ENUM

scalar Money @goModel(model: "example.com/pkg/schematype.Decimal")
type Account @goModel(model: "example.com/pkg/entity.Account") { id: ID! name: String! }
type Loose { id: ID! }
type Query { account(id: ID!): Account }
`

// A schema migrating from another generator already carries its bindings; one
// real schema has 3 778 of them. Restating those in Models is work with no
// decision in it, so the directive's spelling is declared and the bindings are
// read from where they already are.
func TestModelDirectiveIsRead(t *testing.T) {
	dir := writeSchema(t, goModelSDL)
	b, err := newBuilder(dir, Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "example.com/x/graph",
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Money":   "example.com/pkg/schematype.Decimal",
		"Account": "example.com/pkg/entity.Account",
	} {
		if got := b.cfg.Models[name]; got != want {
			t.Errorf("Models[%q] = %q, want %q", name, got, want)
		}
	}
	// A type without the directive is untouched, so the generator still writes
	// a model for it.
	if got, ok := b.cfg.Models["Loose"]; ok {
		t.Errorf("Models[Loose] = %q, want no entry", got)
	}
}

// Naming no directive leaves the schema read exactly as before.
func TestModelDirectiveOffByDefault(t *testing.T) {
	dir := writeSchema(t, goModelSDL)
	b, err := newBuilder(dir, Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "example.com/x/graph",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := b.cfg.Models["Account"]; ok {
		t.Errorf("Models[Account] = %q with no ModelDirective configured", got)
	}
}

// A config file is a deliberate override of what the SDL happens to say.
func TestModelsBeatsTheDirective(t *testing.T) {
	dir := writeSchema(t, goModelSDL)
	b, err := newBuilder(dir, Config{
		Dir: dir, SchemaGlobs: []string{"schema/*.graphql"},
		Output: "graph", Package: "example.com/x/graph",
		Models:         map[string]string{"Account": "example.com/other.Account"},
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.cfg.Models["Account"]; got != "example.com/other.Account" {
		t.Errorf("Models[Account] = %q, want the config's entry to win", got)
	}
}
