package codegen

import (
	"strings"
	"testing"
)

const jsonTagSDL = `
directive @goTag(key: String!, value: String!) repeatable on INPUT_FIELD_DEFINITION
type Thing { id: ID! name: String count: Int! }
input CreateThing {
  title: String! @goTag(key: "valid", value: "required")
  note: String @goTag(key: "valid", value: "omitempty,max=10")
  plain: Int
}
type Query { thing(input: CreateThing): Thing }
`

// Reflection over a model finds a field by its json tag: required fields carry the bare name
// and nullable ones add omitempty, as gqlgen wrote them.
func TestJSONTagsOnObjectAndInputFields(t *testing.T) {
	dir := generate(t, jsonTagSDL, Config{JSONTags: true, TagDirective: goTag})
	models := norm(generated(t, dir, "model/models.go"))

	for _, want := range []string{
		// object fields: only the json tag
		"ID graphql.ID `json:\"id\"`", "Name *string `json:\"name,omitempty\"`", "Count int `json:\"count\"`",
		// input fields: graphql, then json, then the directive tags
		"`graphql:\"title\" json:\"title\" valid:\"required\"`",
		"`graphql:\"note\" json:\"note,omitempty\" valid:\"omitempty,max=10\"`",
		"`graphql:\"plain\" json:\"plain,omitempty\"`",
	} {
		if !strings.Contains(models, norm(want)) {
			t.Errorf("model missing %s:\n%s", want, models)
		}
	}
}

func TestJSONTagsAreOffByDefault(t *testing.T) {
	dir := generate(t, jsonTagSDL, Config{})
	if models := generated(t, dir, "model/models.go"); strings.Contains(models, "json:") {
		t.Errorf("no json tags expected without Config.JSONTags:\n%s", models)
	}
}
