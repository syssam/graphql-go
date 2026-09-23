package codegen

import (
	"strings"
	"testing"
)

// @goField(forceResolver: true) is the author saying the value is computed,
// not read. AutoBind verifying that a struct field exists does not answer
// that: the field may be a column today and a permission-filtered calculation
// tomorrow, and binding it to the column compiles and answers the wrong thing.
// One real schema carries 4 567 of them.
func TestForceResolverDirectiveIsHonoured(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goField(forceResolver: Boolean) on FIELD_DEFINITION
type Doc {
  id: ID!
  title: String!
  total: Int! @goField(forceResolver: true)
  subtotal: Int! @goField(forceResolver: false)
}
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Doc struct {
	ID       string
	Title    string
	Total    int
	Subtotal int
}
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{
		FieldDirective: FieldDirective{Name: "goField", ForceResolverArg: "forceResolver"},
	})

	if strings.Contains(src, `graphql.Field("total"`) {
		t.Errorf("total was bound as a struct field despite forceResolver\n%s", src)
	}
	// Both halves: the interface must demand it and the binding must call it,
	// or the generated file will not compile.
	if !strings.Contains(src, "DocTotal(ctx context.Context, obj *ent.Doc) (int, error)") {
		t.Errorf("total is not on the Resolver interface\n%s", src)
	}
	if !strings.Contains(src, `graphql.Resolve("total"`) {
		t.Errorf("total is not bound through Resolve\n%s", src)
	}
	// false is not true, and an ordinary field is untouched.
	if !strings.Contains(src, `graphql.Field("subtotal", func(v *ent.Doc) int { return v.Subtotal })`) {
		t.Errorf("forceResolver: false was read as true\n%s", src)
	}
	if !strings.Contains(src, `graphql.Field("title", func(v *ent.Doc) string { return v.Title })`) {
		t.Errorf("an undirected field stopped binding\n%s", src)
	}
}

// Off by default, like ModelDirective: a schema that happens to carry a
// directive of that name gets nothing from it until the config asks.
func TestFieldDirectiveOffByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goField(forceResolver: Boolean) on FIELD_DEFINITION
type Doc { id: ID! total: Int! @goField(forceResolver: true) }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Doc struct {
	ID    string
	Total int
}
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{})
	if !strings.Contains(src, `graphql.Field("total", func(v *ent.Doc) int { return v.Total })`) {
		t.Errorf("the directive was read without being configured\n%s", src)
	}
}
