package codegen

import (
	"context"
	"go/token"
	"strings"
	"testing"
)

// A group is a Go package named after an SDL file, so func.graphql asked for
// `package func`, main.graphql for a package nothing can import, and
// graphql.graphql, context.graphql and embed.graphql for a package whose name
// the generated code already uses for an import.
func TestAGroupIsNeverNamedSomethingGoOrTheGeneratorReserves(t *testing.T) {
	for _, stem := range []string{"func", "type", "go", "range", "select", "main", "graphql", "context", "embed"} {
		got := sanitizeGroup(stem, "graph")
		if got == stem || token.IsKeyword(got) {
			t.Errorf("sanitizeGroup(%q) = %q, which cannot name a generated package", stem, got)
		}
	}
	// Ordinary names are left alone.
	if got := sanitizeGroup("user", "graph"); got != "user" {
		t.Errorf("sanitizeGroup(%q) = %q", "user", got)
	}
}

// An enum's constants are named Type+Value, which is also a legal type name:
// enum Color { RED } beside type ColorRed generated two declarations of
// ColorRed, found by the compiler in a file the author is told not to edit.
func TestAnEnumConstantThatCollidesWithATypeIsRefused(t *testing.T) {
	_, err := generateWith(t, `
		enum Color { RED GREEN }
		type ColorRed { id: ID! }
		input ColorGreen { id: ID! }
		type Query { c: Color r: ColorRed g(in: ColorGreen): Int }
	`, Config{})
	if err == nil {
		t.Fatal("generated two Go declarations of one name")
	}
	for _, want := range []string{"Color.RED", "ColorRed", "Color.GREEN", "ColorGreen"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

const jsonAndGoTagSDL = `
directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION
input CreateThing {
  name: String @goTag(key: "json", value: "custom_name")
  other: String
}
type Query { thing(input: CreateThing): String }
`

// JSONTags and a @goTag for json each wrote a json key, and reflect answers
// with the first: the tag the author spelled out was silently dead.
func TestATagDirectiveForJSONReplacesTheGeneratedJSONTag(t *testing.T) {
	dir := generate(t, jsonAndGoTagSDL, Config{TagDirective: goTag, JSONTags: true})
	models := norm(generated(t, dir, "model/models.go"))
	if !strings.Contains(models, norm("`graphql:\"name\" json:\"custom_name\"`")) {
		t.Errorf("the directive's json tag is not the field's only one:\n%s", models)
	}
	if !strings.Contains(models, norm("`graphql:\"other\" json:\"other,omitempty\"`")) {
		t.Errorf("a field without the directive lost its generated json tag:\n%s", models)
	}
}

func TestTagDirectiveRefusesWhatAStructTagCannotCarry(t *testing.T) {
	for name, directive := range map[string]string{
		"control character in the key":   `@goTag(key: "v\u0001", value: "x")`,
		"control character in the value": `@goTag(key: "v", value: "a\u0000b")`,
		"a null value":                   `@goTag(key: "v", value: null)`,
	} {
		t.Run(name, func(t *testing.T) {
			sdl := `
				directive @goTag(key: String!, value: String) repeatable on INPUT_FIELD_DEFINITION
				input In { name: String ` + directive + ` }
				type Query { f(in: In): Int }`
			_, err := generateWith(t, sdl, Config{TagDirective: goTag})
			if err == nil {
				t.Fatal("generated a struct tag from it")
			}
			if !strings.Contains(err.Error(), "In.name") || strings.Contains(err.Error(), "package model") {
				t.Errorf("want an error naming In.name, not a dump of the generated source: %v", err)
			}
		})
	}
}

// A FieldNames key that names nothing in the schema is a typo, and was
// accepted in silence where every other configuration key is checked.
func TestFieldNamesRefusesANameTheSchemaDoesNotHave(t *testing.T) {
	sdl := `type T { userId: ID! } type Query { t(byId: ID): T }`
	if _, err := generateWith(t, sdl, Config{FieldNames: map[string]string{"userId": "UserID", "byId": "ByID"}}); err != nil {
		t.Fatalf("a field name and an argument name were refused: %v", err)
	}
	first := ""
	for range 20 {
		_, err := generateWith(t, sdl, Config{FieldNames: map[string]string{"usrId": "UserID", "zzz": "Z", "aaa": "A"}})
		if err == nil {
			t.Fatal("FieldNames keys that name no field or argument were accepted")
		}
		for _, want := range []string{"usrId", "zzz", "aaa"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error does not name %s: %v", want, err)
			}
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("the same configuration gave two errors:\n%s\n%s", first, err)
		}
	}
}

// The engine binds a scalar or enum through two list levels and an input
// object through one. SDL nesting deeper than that generated Go that compiled
// and a schema that could not be built, with nothing at generate time to say
// so -- and no Go type the author could substitute, since the limit is on the
// position, not on the model.
func TestListsNestedDeeperThanTheEngineBindsAreRefused(t *testing.T) {
	_, err := generateWith(t, `
		input Point { x: Float! y: Float! }
		input Poly { rings: [[Point!]!]  ok: [Point!] }
		type Geo { coordinates: [[[Float!]!]!]!  ok: [[Float!]!]! }
		type Query { geo: Geo  grid(cells: [[[Int]]], poly: Poly): Int }
	`, Config{})
	if err == nil {
		t.Fatal("generated bindings NewSchema cannot build")
	}
	for _, want := range []string{"Poly.rings", "Geo.coordinates", "Query.grid(cells:)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	for _, fine := range []string{"Poly.ok", "Geo.ok"} {
		if strings.Contains(err.Error(), fine) {
			t.Errorf("%s is within what the engine binds and was refused: %v", fine, err)
		}
	}
}

// What the engine does bind still generates, including an object list of any
// depth, which goes through the reflection traverser.
func TestListsTheEngineBindsStillGenerate(t *testing.T) {
	if _, err := generateWith(t, `
		input Point { x: Float! }
		type Cell { id: ID! }
		type Query { a(p: [Point!], m: [[Int!]!]): [[Float!]!]  grid: [[[Cell!]!]!] }
	`, Config{}); err != nil {
		t.Fatalf("refused lists the engine binds: %v", err)
	}
}

// A package path whose last element is not an identifier, or a model that is
// not a Go type expression, was written into the output as it stood, and the
// error was gofmt's: a line and column in a file that was never written,
// followed by the whole of its source.
func TestConfigThatCannotBeGoIsRefusedByName(t *testing.T) {
	sdl := `type User { id: ID! } type Query { me: User }`
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"package is not an identifier": {Config{}, "Package"},
		"package is a keyword":         {Config{}, "Package"},
		"model is not a type":          {Config{Models: map[string]string{"User": "example.com/app.not a type!"}}, "Models"},
		"model has no type name":       {Config{Models: map[string]string{"User": "example.com/app."}}, "Models"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"schema.graphql": sdl})
			cfg := tc.cfg
			cfg.Dir, cfg.SchemaGlobs, cfg.Output, cfg.Package = dir, []string{"schema.graphql"}, "graph", "hello/graph"
			switch name {
			case "package is not an identifier":
				cfg.Package = "hello/1-graph"
			case "package is a keyword":
				cfg.Package = "hello/func"
			}
			err := Generate(context.Background(), cfg)
			if err == nil {
				t.Fatal("generated from configuration that cannot be Go")
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "DO NOT EDIT") {
				t.Errorf("want an error about %s, not a dump of generated source: %v", tc.want, err)
			}
		})
	}
}
