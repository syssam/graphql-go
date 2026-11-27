package codegen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tagSDL = `
directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION
input CreateThing {
  name: String @goTag(key: "valid", value: "required,max=200")
  email: String @goTag(key: "valid", value: "omitempty,email") @goTag(key: "label", value: "E-mail")
  plain: Int
}
type Query { thing(input: CreateThing): String }
`

var goTag = TagDirective{Name: "goTag", KeyArg: "key", ValueArg: "value"}

// generateWith is generate, but hands the error back so a rejection can be asserted.
func generateWith(t *testing.T, sdl string, cfg Config) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Dir = dir
	cfg.SchemaGlobs = []string{"schema.graphql"}
	cfg.Output = "graph"
	cfg.Package = "hello/graph"
	return dir, Generate(context.Background(), cfg)
}

// A service that validates the decoded input with struct tags needs the tags the SDL
// declares. They follow the graphql tag, in directive order, and a field without the
// directive keeps only the graphql tag.
func TestTagDirectiveWritesTagsOnInputFields(t *testing.T) {
	t.Parallel()
	dir := generate(t, tagSDL, Config{TagDirective: goTag})
	models := norm(generated(t, dir, "model/models.go"))

	for _, want := range []string{
		"`graphql:\"name\" valid:\"required,max=200\"`",
		"`graphql:\"email\" valid:\"omitempty,email\" label:\"E-mail\"`",
		"`graphql:\"plain\"`",
	} {
		if !strings.Contains(models, norm(want)) {
			t.Errorf("model missing %s:\n%s", want, models)
		}
	}
}

// Unconfigured, the output is what it was before the option existed: a directive in the SDL
// is not read.
func TestTagDirectiveIsOffByDefault(t *testing.T) {
	t.Parallel()
	dir := generate(t, tagSDL, Config{})
	models := generated(t, dir, "model/models.go")
	if strings.Contains(models, "valid:") || strings.Contains(models, "label:") {
		t.Errorf("no extra tags expected without Config.TagDirective:\n%s", models)
	}
}

// A tag the struct-tag syntax cannot carry would be dropped silently by reflect or mean
// something else, so each is a generation error that names the field.
func TestTagDirectiveRejectsWhatATagCannotCarry(t *testing.T) {
	t.Parallel()
	sdl := func(field string) string {
		return "directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION\n" +
			"input In { " + field + " }\ntype Query { q(i: In): String }\n"
	}
	tests := []struct {
		name, field, want string
	}{
		{"a quote in the value", `a: String @goTag(key: "valid", value: "min=\"1\"")`, "quote"},
		{"a backtick in the value", "a: String @goTag(key: \"valid\", value: \"x`y\")", "backtick"},
		{"a backslash in the value", `a: String @goTag(key: "valid", value: "a\\b")`, "backslash"},
		{"a space in the key", `a: String @goTag(key: "my key", value: "x")`, "struct-tag key"},
		{"a colon in the key", `a: String @goTag(key: "a:b", value: "x")`, "struct-tag key"},
		{"an empty key", `a: String @goTag(key: "", value: "x")`, "struct-tag key"},
		{"the reserved graphql key", `a: String @goTag(key: "graphql", value: "x")`, "reserved"},
		{"a key repeated on one field", `a: String @goTag(key: "valid", value: "x") @goTag(key: "valid", value: "y")`, "repeats"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := generateWith(t, sdl(tc.field), Config{TagDirective: goTag})
			if err == nil {
				t.Fatal("expected a generation error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "In.a") {
				t.Errorf("error should mention %q and name In.a, got: %v", tc.want, err)
			}
		})
	}
}

// The same inputs are fine when the option is not configured: the directive is just an
// annotation nobody reads, so existing users see no change.
func TestTagDirectiveIgnoredWhenUnconfiguredEvenIfMalformed(t *testing.T) {
	t.Parallel()
	sdl := "directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION\n" +
		"input In { a: String @goTag(key: \"valid\", value: \"x`y\") }\ntype Query { q(i: In): String }\n"
	if _, err := generateWith(t, sdl, Config{}); err != nil {
		t.Fatalf("an unconfigured directive must not be validated: %v", err)
	}
}

func TestTagDirectiveIsZero(t *testing.T) {
	t.Parallel()
	for _, d := range []TagDirective{{}, {Name: "goTag"}, {Name: "goTag", KeyArg: "key"}, {KeyArg: "key", ValueArg: "value"}} {
		if !d.IsZero() {
			t.Errorf("%+v should be zero", d)
		}
	}
	if goTag.IsZero() {
		t.Error("a fully named directive is not zero")
	}
}

const argTagSDL = `
directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION | ARGUMENT_DEFINITION
type Query {
  search(term: String! @goTag(key: "valid", value: "required,max=50"), page: Int): String
}
`

// A directive the SDL accepts on an argument was read by nobody, so a service that
// validated its Args struct with tags got none and found out at runtime. The tag goes
// where it does on an input field: after the graphql tag, in directive order.
func TestTagDirectiveWritesTagsOnArguments(t *testing.T) {
	t.Parallel()
	dir := generate(t, argTagSDL, Config{TagDirective: goTag})
	all := norm(generatedSources(t, dir))
	for _, want := range []string{
		"`graphql:\"term\" valid:\"required,max=50\"`",
		"`graphql:\"page\"`",
	} {
		if !strings.Contains(all, norm(want)) {
			t.Errorf("generated code missing %s:\n%s", want, all)
		}
	}
}

func TestTagDirectiveOnArgumentsIsOffByDefault(t *testing.T) {
	t.Parallel()
	dir := generate(t, argTagSDL, Config{})
	if all := generatedSources(t, dir); strings.Contains(all, "valid:") {
		t.Errorf("no extra tags expected without Config.TagDirective:\n%s", all)
	}
}

// An argument's tag is held to the rule an input field's is: one reflect would drop is a
// generation error that names the argument, on an object and on an interface alike.
func TestTagDirectiveRejectsWhatATagCannotCarryOnArguments(t *testing.T) {
	t.Parallel()
	const decl = "directive @goTag(key: String!, value: String!) repeatable on ARGUMENT_DEFINITION\n"
	tests := []struct{ name, sdl, coord, want string }{
		{"a quote in the value on an object", decl + `type Query { q(a: String @goTag(key: "valid", value: "min=\"1\"")): String }`, "Query.q(a)", "quote"},
		{"a reserved key on an object", decl + `type Query { q(a: String @goTag(key: "graphql", value: "x")): String }`, "Query.q(a)", "reserved"},
		{"a repeated key on an object", decl + `type Query { q(a: String @goTag(key: "v", value: "x") @goTag(key: "v", value: "y")): String }`, "Query.q(a)", "repeats"},
		{"a bad key on an interface", decl + `interface Node { f(a: String @goTag(key: "my key", value: "x")): String } type T implements Node { f(a: String): String } type Query { t: T }`, "Node.f(a)", "struct-tag key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := generateWith(t, tc.sdl, Config{TagDirective: goTag})
			if err == nil {
				t.Fatal("expected a generation error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.coord) {
				t.Errorf("error should mention %q and name %s, got: %v", tc.want, tc.coord, err)
			}
		})
	}
}

// A tag the emitter writes is only worth having if what it writes compiles and the
// engine still reads the argument by its graphql tag beside the new one.
func TestTagDirectiveOnArgumentsCompiles(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping package build")
	}
	dir := writeEntModule(t, argTagSDL, "package ent\n")
	if err := Generate(context.Background(), Config{
		Dir: dir, SchemaGlobs: []string{"schema.graphql"},
		Output: "graph", Package: "hello/graph",
		TagDirective: goTag,
	}); err != nil {
		t.Fatal(err)
	}
	if src := norm(generatedSources(t, dir)); !strings.Contains(src, norm("`graphql:\"term\" valid:\"required,max=50\"`")) {
		t.Fatalf("the tag is not in the output:\n%s", src)
	}
	buildGenerated(t, dir)
}
