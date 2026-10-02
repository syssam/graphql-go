package codegen

import (
	"strings"
	"testing"
)

const fieldNamesSDL = `
input UpdateThing { _lastUpdatedAt: String! name: String }
type Query { thing(_lastUpdatedAt: String, id: ID): String  update(input: UpdateThing): String }
`

var lastUpdated = map[string]string{"_lastUpdatedAt": "LastUpdatedAt"}

// A leading underscore keeps an X by default; FieldNames chooses the plain name instead, for the
// input field and for an argument of the same SDL name, and the tag still pins the SDL name.
func TestFieldNamesOverrideTheDerivedName(t *testing.T) {
	dir := generate(t, fieldNamesSDL, Config{FieldNames: lastUpdated})
	models := norm(generated(t, dir, "model/models.go"))
	if !strings.Contains(models, norm("LastUpdatedAt string `graphql:\"_lastUpdatedAt\"`")) {
		t.Errorf("input field should be LastUpdatedAt:\n%s", models)
	}
	if strings.Contains(models, "XLastUpdatedAt") {
		t.Errorf("the derived X name must not appear when overridden:\n%s", models)
	}
	if args := norm(generated(t, dir, "generated.go")); !strings.Contains(args, norm("LastUpdatedAt *string `graphql:\"_lastUpdatedAt\"`")) {
		t.Errorf("argument should be LastUpdatedAt too:\n%s", args)
	}
}

func TestFieldNamesKeepTheDerivedNameByDefault(t *testing.T) {
	dir := generate(t, fieldNamesSDL, Config{})
	if models := generated(t, dir, "model/models.go"); !strings.Contains(models, "XLastUpdatedAt") {
		t.Errorf("default must keep the X prefix:\n%s", models)
	}
}

func TestFieldNamesRejectsAnUnexportedOrInvalidName(t *testing.T) {
	for _, bad := range []string{"lastUpdatedAt", "", "Last Updated", "1Last"} {
		_, err := generateWith(t, fieldNamesSDL, Config{FieldNames: map[string]string{"_lastUpdatedAt": bad}})
		if err == nil || !strings.Contains(err.Error(), "exported Go identifier") {
			t.Errorf("FieldNames value %q should be refused, got %v", bad, err)
		}
	}
}

// The collision check runs on the final names, so an override that makes two fields on one type
// the same identifier is refused with their SDL coordinates, like any other collision.
func TestFieldNamesCollisionIsStillRefused(t *testing.T) {
	sdl := "input In { _lastUpdatedAt: String lastUpdatedAt: String }\ntype Query { q(i: In): String }\n"
	_, err := generateWith(t, sdl, Config{FieldNames: lastUpdated})
	if err == nil {
		t.Fatal("two fields mapped to LastUpdatedAt were accepted")
	}
	for _, want := range []string{"In._lastUpdatedAt", "In.lastUpdatedAt", `"LastUpdatedAt"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got: %v", want, err)
		}
	}
}

// A bare key renames that name wherever it appears, which is the wrong tool
// when one type's field needs a name and another's does not. A key written as
// a schema coordinate applies to that field or argument alone, and wins over
// a bare key for the same name.
func TestFieldNamesTakesASchemaCoordinate(t *testing.T) {
	sdl := `
		type User { userId: ID! }
		type Post { userId: ID! }
		input Filter { userId: ID }
		type Query { user(byId: ID): User post(byId: ID): Post find(f: Filter): Int }
	`
	dir := generate(t, sdl, Config{FieldNames: map[string]string{
		"User.userId":       "UID",
		"Query.user(byId:)": "Key",
		"Filter.userId":     "Owner",
		"byId":              "ByID",
	}})
	models := norm(generated(t, dir, "model/models.go"))
	for _, want := range []string{"type User struct { UID", "type Post struct { UserID", "Owner *"} {
		if !strings.Contains(models, norm(want)) {
			t.Errorf("models.go lacks %q:\n%s", want, models)
		}
	}
	src := norm(generated(t, dir, "generated.go"))
	for _, want := range []string{"type UserArgs struct { Key ", "type PostArgs struct { ByID ", "return v.UID"} {
		if !strings.Contains(src, norm(want)) {
			t.Errorf("generated.go lacks %q:\n%s", want, src)
		}
	}

	_, err := generateWith(t, sdl, Config{FieldNames: map[string]string{"User.nope": "X", "Query.user(nope:)": "Y"}})
	if err == nil || !strings.Contains(err.Error(), "User.nope") || !strings.Contains(err.Error(), "Query.user(nope:)") {
		t.Fatalf("err = %v, want one naming both coordinates that match nothing", err)
	}
}
