package codegen

import (
	"strings"
	"testing"
)

const structObjectsSDL = `
type Report { id: ID! title: String! lines: [Line!]! owner: Owner parent(depth: Int): Report }
type Line { amount: Int! }
type Owner { name: String! }
type Query { report: Report }
`

// A model that services build by hand carries its nested objects in the struct.
// With StructObjectFields a field of object type with no arguments is read from
// the struct like a scalar, as gqlgen did; a field with arguments still needs a
// resolver.
func TestStructObjectFieldsKeepsObjectFieldsOnGeneratedModels(t *testing.T) {
	dir := generate(t, structObjectsSDL, Config{StructObjectFields: true})
	models := generated(t, dir, "model/models.go")
	src := generated(t, dir, "generated.go")

	for _, want := range []string{"Lines []*Line", "Owner *Owner", "Title string"} {
		if !strings.Contains(norm(models), norm(want)) {
			t.Errorf("model Report missing %q:\n%s", want, models)
		}
	}
	if strings.Contains(models, "Parent") {
		t.Errorf("a field with arguments must not become a struct field:\n%s", models)
	}
	for _, want := range []string{`graphql.Field("lines"`, `graphql.Field("owner"`} {
		if !strings.Contains(src, want) {
			t.Errorf("generated.go missing %s:\n%s", want, src)
		}
	}
	if strings.Contains(src, "ReportLines(") || strings.Contains(src, "ReportOwner(") {
		t.Errorf("struct-bound object fields must not be Resolver methods:\n%s", src)
	}
	if !strings.Contains(src, "ReportParent(") {
		t.Errorf("Report.parent takes an argument and must stay a resolver:\n%s", src)
	}
}

// The default is unchanged: object fields are resolvers.
func TestObjectFieldsAreResolversByDefault(t *testing.T) {
	dir := generate(t, structObjectsSDL, Config{})
	src := generated(t, dir, "generated.go")
	if !strings.Contains(src, "ReportLines(") || !strings.Contains(src, "ReportOwner(") {
		t.Errorf("object fields should be Resolver methods by default:\n%s", src)
	}
}

// A manifest entry that names no Go type overrides only the fields it lists.
// Before, appearing in the manifest at all turned every other field into a
// resolver, so forcing one field to a resolver on a generated model emptied it.
func TestManifestEntryWithoutAGoTypeOnlyOverridesItsFields(t *testing.T) {
	dir := generate(t, structObjectsSDL, Config{
		StructObjectFields: true,
		Manifest: &Manifest{Types: []TypeBinding{{
			Name:   "Report",
			Fields: map[string]FieldBinding{"title": {Kind: FieldResolver}},
		}}},
	})
	models := generated(t, dir, "model/models.go")
	src := generated(t, dir, "generated.go")

	if !strings.Contains(src, "ReportTitle(") {
		t.Errorf("the listed field should be a resolver:\n%s", src)
	}
	if strings.Contains(models, "Title string") {
		t.Errorf("the forced field should leave the struct:\n%s", models)
	}
	for _, want := range []string{"ID ", "Lines []*Line"} {
		if !strings.Contains(norm(models), norm(want)) {
			t.Errorf("unlisted %q should still be inferred:\n%s", want, models)
		}
	}
}

// norm collapses runs of spaces so a test need not match gofmt's alignment.
func norm(s string) string { return strings.Join(strings.Fields(s), " ") }
