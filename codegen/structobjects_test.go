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

const inputListSDL = `
input Line { n: Int! }
input Order { lines: [Line!]! maybe: [Line!] tags: [String!] sparse: [Line] }
type Query { echo(lines: [Line!]): Int }
`

// gqlgen spelled a list of input objects []*Input; callers written against it
// test elements for nil and dereference them. InputListPointers keeps that shape.
func TestInputListPointersKeepsPointerElements(t *testing.T) {
	dir := generate(t, inputListSDL, Config{InputListPointers: true})
	models := norm(generated(t, dir, "model/models.go"))
	for _, want := range []string{"Lines []*Line", "Maybe []*Line", "Tags []string", "Sparse []*Line"} {
		if !strings.Contains(models, want) {
			t.Errorf("model missing %q:\n%s", want, models)
		}
	}
	if args := norm(generated(t, dir, "generated.go")); !strings.Contains(args, "Lines []*model.Line") {
		t.Errorf("a field argument list should use pointers too:\n%s", args)
	}
}

// A nullable ELEMENT must not be wrapped in Omittable and then pointed at: the
// element is simply *T.
func TestInputListPointersOnNullableElementsWithOmittable(t *testing.T) {
	dir := generate(t, inputListSDL, Config{InputListPointers: true, NullableInputOmittable: true})
	models := norm(generated(t, dir, "model/models.go"))
	if strings.Contains(models, "*graphql.Omittable") || strings.Contains(models, "[]graphql.Omittable") {
		t.Errorf("list elements must not be Omittable:\n%s", models)
	}
	if !strings.Contains(models, "Sparse graphql.Omittable[*[]*Line]") && !strings.Contains(models, "Sparse []*Line") {
		t.Errorf("Sparse should be a list of *Line:\n%s", models)
	}
}

func TestInputListsAreValuesByDefault(t *testing.T) {
	dir := generate(t, inputListSDL, Config{})
	if models := norm(generated(t, dir, "model/models.go")); !strings.Contains(models, "Lines []Line") {
		t.Errorf("default should keep value elements:\n%s", models)
	}
}
