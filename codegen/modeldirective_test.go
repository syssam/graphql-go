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

// AutoBind discovers; Models and a binding directive declare. Before
// ModelDirective these two never met on this path, because Models was empty
// whenever AutoBind ran -- so a discovery disagreeing with a declaration
// surfaced as "type X is bound to A but Models says B" and stopped generation
// on a real schema.
//
// A disagreement between two declarations is still an error. This is the other
// case: the declaration wins, quietly, and the discovery keeps everything else
// it learned.
func TestDiscoveryYieldsToADeclaredBinding(t *testing.T) {
	discovered := &Manifest{Types: []TypeBinding{
		{
			Name:   "Account",
			Go:     GoType{PkgPath: "example.com/pkg/schematype", Name: "Account"},
			Fields: map[string]FieldBinding{"name": {Kind: FieldStruct, GoName: "Name"}},
		},
		{Name: "Loose", Go: GoType{PkgPath: "example.com/pkg/schematype", Name: "Loose"}},
	}}
	models := map[string]string{"Account": "example.com/other/entity.Account"}

	out := yieldToDeclared(discovered, models)

	byName := map[string]TypeBinding{}
	for _, tb := range out.Types {
		byName[tb.Name] = tb
	}
	if got := byName["Account"]; !got.Go.zero() {
		t.Errorf("Account kept the discovered Go type %q; the declaration must win", got.Go.expr())
	}
	// Everything else the discovery learned survives: dropping the whole
	// binding would send every field of the type back to the Resolver.
	if got := byName["Account"].Fields["name"]; got.GoName != "Name" {
		t.Errorf("Account.name lost its discovered field binding: %+v", got)
	}
	// A type nobody declared keeps what was discovered.
	if got := byName["Loose"]; got.Go.zero() {
		t.Error("Loose lost its discovered Go type though nothing declared it")
	}

	// The input is not mutated: callers may still hold it.
	if discovered.Types[0].Go.zero() {
		t.Error("yieldToDeclared wrote through to its argument")
	}
}

// The wiring, not just the function: a schema that declares a binding in the
// SDL and also autobinds must generate, with the declaration winning.
//
// The unit test above passes whether or not yieldToDeclared is called, which
// is exactly the shape of test this repository warns about. This one goes
// through newBuilder and fails with "is bound to ... but Models says ..."
// when the call is removed.
func TestDiscoveryYieldsThroughTheBuilder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goModel(model: String) on OBJECT
type Product @goModel(model: "hello/other.Product") { id: ID! title: String! }
type Query { product(id: ID!): Product }
`
	const source = `package ent

type Product struct {
	ID    string
	Title string
}
`
	dir := writeEntModule(t, sdl, source)
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other", "product.go"),
		[]byte("package other\n\ntype Product struct {\n\tID    string\n\tTitle string\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := newBuilder(dir, Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		AutoBind:       []string{"./ent"},
		ModelDirective: ModelDirective{Name: "goModel", Arg: "model"},
	})
	if err != nil {
		t.Fatalf("a declared binding and a discovered one were treated as a conflict: %v", err)
	}
	if got := b.cfg.Models["Product"]; got != "hello/other.Product" {
		t.Errorf("Models[Product] = %q, want the SDL's declaration to win", got)
	}
}
