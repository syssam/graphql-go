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
	dir := generate(t, tagSDL, Config{})
	models := generated(t, dir, "model/models.go")
	if strings.Contains(models, "valid:") || strings.Contains(models, "label:") {
		t.Errorf("no extra tags expected without Config.TagDirective:\n%s", models)
	}
}

// A tag the struct-tag syntax cannot carry would be dropped silently by reflect or mean
// something else, so each is a generation error that names the field.
func TestTagDirectiveRejectsWhatATagCannotCarry(t *testing.T) {
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
	sdl := "directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION\n" +
		"input In { a: String @goTag(key: \"valid\", value: \"x`y\") }\ntype Query { q(i: In): String }\n"
	if _, err := generateWith(t, sdl, Config{}); err != nil {
		t.Fatalf("an unconfigured directive must not be validated: %v", err)
	}
}

func TestTagDirectiveIsZero(t *testing.T) {
	for _, d := range []TagDirective{{}, {Name: "goTag"}, {Name: "goTag", KeyArg: "key"}, {KeyArg: "key", ValueArg: "value"}} {
		if !d.IsZero() {
			t.Errorf("%+v should be zero", d)
		}
	}
	if goTag.IsZero() {
		t.Error("a fully named directive is not zero")
	}
}
