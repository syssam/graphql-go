package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const presetSDL = `
directive @goTag(key: String!, value: String) repeatable on INPUT_FIELD_DEFINITION
input Filter { name: String @goTag(key: "valid", value: "required") }
input Search { filters: [Filter!] }
type Owner { name: String! }
type Report { id: ID! owner: Owner }
type Query { report(s: Search): Report }
`

// A schema coming from gqlgen needs six settings to generate what gqlgen
// generated, each of which is silent when missing: the code compiles and the
// models are simply a different shape. One key turns them on together.
func TestPresetGqlgenTurnsOnWhatGqlgenDid(t *testing.T) {
	dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\npreset: gqlgen\n", presetSDL)
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	models := strings.Join(strings.Fields(generated(t, dir, "graph/model/models.go")), " ")
	for what, want := range map[string]string{
		"jsonTags":           "json:\"name,omitempty\"",
		"tagDirective":       "valid:\"required\"",
		"inputListPointers":  "Filters []*Filter",
		"structObjectFields": "Owner *Owner",
	} {
		if !strings.Contains(models, want) {
			t.Errorf("preset: gqlgen did not turn on %s; models.go lacks %q:\n%s", what, want, models)
		}
	}
}

// A key written out beside the preset wins over it, in either direction.
func TestAnExplicitKeyOverridesThePreset(t *testing.T) {
	dir, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\npreset: gqlgen\njsonTags: false\ninputListPointers: false\n", presetSDL)
	if err := run([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	models := strings.Join(strings.Fields(generated(t, dir, "graph/model/models.go")), " ")
	if strings.Contains(models, "json:\"") {
		t.Errorf("jsonTags: false did not override the preset:\n%s", models)
	}
	if strings.Contains(models, "[]*Filter") {
		t.Errorf("inputListPointers: false did not override the preset:\n%s", models)
	}
	if !strings.Contains(models, "valid:\"required\"") {
		t.Errorf("the rest of the preset was lost with the override:\n%s", models)
	}
}

func TestAnUnknownPresetIsRefused(t *testing.T) {
	_, cfg := write(t, "schema: [schema.graphql]\noutput: graph\npackage: example/graph\npreset: gqlgem\n", presetSDL)
	err := run([]string{"-config", cfg})
	if err == nil || !strings.Contains(err.Error(), "gqlgem") || !strings.Contains(err.Error(), "gqlgen") {
		t.Fatalf("err = %v, want one naming the preset given and the one that exists", err)
	}
}

// The wire key reaches the generator: its effect is a wire.go.
func TestWireKeyReachesTheGenerator(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"schema/user.graphql": "type User { id: ID! }\ntype Query { users: [User!]! }\n",
		"schema/post.graphql": "type Post { id: ID! }\nextend type Query { posts: [Post!]! }\n",
		"gqlc.yaml": "schema: [schema/*.graphql]\noutput: graph\npackage: example/graph\n" +
			"scaffold:\n  user: graph/user.Handler\n  post: graph/post.Handler\n" +
			"wire:\n  deps: example/internal/deps.Deps\n  pass:\n    user: \"&d\"\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"-config", filepath.Join(dir, "gqlc.yaml")}); err != nil {
		t.Fatal(err)
	}
	src := generated(t, dir, "graph/wire.go")
	if !strings.Contains(src, "func Wire(d deps.Deps)") {
		t.Fatalf("the wire key did not reach the generator:\n%s", src)
	}
	if !strings.Contains(src, "user.NewHandler(&d)") || !strings.Contains(src, "post.NewHandler(d)") {
		t.Fatalf("wire.pass did not reach the generator, or reached a group it does not name:\n%s", src)
	}
}

// A directive key written in part beside the preset keeps what was written
// and takes the rest from the preset. Merged all or nothing on the name, a
// lone `arg` was thrown away, and a lone `name` left the other argument empty,
// which turns the directive off without a word.
func TestThePresetFillsOnlyWhatADirectiveKeyLeftOut(t *testing.T) {
	fc := fileConfig{Preset: "gqlgen"}
	fc.ModelDirective.Arg = "type"
	fc.FieldDirective.Name = "goField"
	fc.TagDirective.ValueArg = "v"
	if err := fc.applyPreset(); err != nil {
		t.Fatal(err)
	}
	if fc.ModelDirective.Name != "goModel" || fc.ModelDirective.Arg != "type" {
		t.Errorf("modelDirective = %+v, want the written arg kept and the name filled in", fc.ModelDirective)
	}
	if fc.FieldDirective.Name != "goField" || fc.FieldDirective.ForceResolverArg != "forceResolver" {
		t.Errorf("fieldDirective = %+v, want the missing argument filled in", fc.FieldDirective)
	}
	if fc.TagDirective.Name != "goTag" || fc.TagDirective.KeyArg != "key" || fc.TagDirective.ValueArg != "v" {
		t.Errorf("tagDirective = %+v", fc.TagDirective)
	}
}
