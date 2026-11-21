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

var goFieldNamed = FieldDirective{Name: "goField", ForceResolverArg: "forceResolver", NameArg: "name"}

// @goField(name: "X") on a type bound to Go says which Go field or method
// answers, so it is the only name looked up, and exactly: not the SDL name,
// not a json tag naming the field, not a case-insensitive match. A name the
// type lacks leaves a Resolver method of that name. The output must compile.
func TestFieldDirectiveNameBindsTheNamedGoField(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
directive @goField(forceResolver: Boolean, name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
type Doc {
  id: ID!
  total: Int! @goField(name: "GrandTotal")
  label: String! @goField(name: "Caption")
  summary: String! @goField(name: "Computed")
  count: Int! @goField(name: "COUNT")
  title: String!
}
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type Doc struct {
	ID         string
	Total      int
	GrandTotal int
	Other      int ` + "`json:\"total\"`" + `
	Label      string
	Summary    string
	Count      int
	Title      string
}

func (d *Doc) Computed() string { return d.Title }
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{FieldDirective: goFieldNamed})

	for _, want := range []string{
		`graphql.Field("total", func(v *ent.Doc) int { return v.GrandTotal })`,
		`graphql.Field("summary", func(v *ent.Doc) string { return v.Computed() })`,
		`graphql.Field("title", func(v *ent.Doc) string { return v.Title })`,
		"DocCaption(ctx context.Context, obj *ent.Doc) (string, error)",
		// Exactly the name written: COUNT is not Count.
		"DocCOUNT(ctx context.Context, obj *ent.Doc) (int, error)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s\n%s", want, src)
		}
	}
	if strings.Contains(src, "v.Label") {
		t.Errorf("label was bound by its SDL name although the directive named Caption\n%s", src)
	}
	goBuild(t, dir)
}

// On a model gqlc writes, the name is the Go field's name, on the object and
// the input alike, and the binding reads it. A FieldNames entry for the same
// coordinate wins, as Models wins over the model directive.
func TestFieldDirectiveNameNamesAGeneratedModelField(t *testing.T) {
	const sdl = `
directive @goField(forceResolver: Boolean, name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
type Plan { maxStorageGb: Int! @goField(name: "MaxStorageGB") seats: Int! @goField(name: "SeatCount") }
input PlanInput { maxStorageGb: Int @goField(name: "MaxStorageGB") }
type Query { plan(in: PlanInput): Plan }
`
	dir := generate(t, sdl, Config{
		FieldDirective: goFieldNamed,
		FieldNames:     map[string]string{"Plan.seats": "Seats"},
	})
	models := norm(generated(t, dir, "model/models.go"))
	for _, want := range []string{
		"type Plan struct { MaxStorageGB int Seats int }",
		norm("MaxStorageGB *int `graphql:\"maxStorageGb\"`"),
	} {
		if !strings.Contains(models, want) {
			t.Errorf("models lack %s\n%s", want, models)
		}
	}
	if strings.Contains(models, "MaxStorageGb") || strings.Contains(models, "SeatCount") {
		t.Errorf("a derived name or an overridden directive name survived\n%s", models)
	}
	if src := generated(t, dir, "generated.go"); !strings.Contains(src, "v.MaxStorageGB") {
		t.Errorf("the binding does not read MaxStorageGB\n%s", src)
	}
}

// The name argument alone switches the directive on: IsZero must not ask for
// ForceResolverArg too.
func TestFieldDirectiveNameArgAlone(t *testing.T) {
	const sdl = `
directive @goField(name: String) on FIELD_DEFINITION
type Plan { maxStorageGb: Int! @goField(name: "MaxStorageGB") }
type Query { plan: Plan }
`
	dir := generate(t, sdl, Config{FieldDirective: FieldDirective{Name: "goField", NameArg: "name"}})
	if models := generated(t, dir, "model/models.go"); !strings.Contains(models, "MaxStorageGB") {
		t.Errorf("name: was not read with only NameArg set\n%s", models)
	}
}

// An argument the configuration does not read is refused with the field's
// coordinate and what to do instead. Dropped, name: derived another Go name
// and omittable: changed nothing, both silently.
func TestFieldDirectiveRefusesAnArgumentItDoesNotRead(t *testing.T) {
	const decl = "directive @goField(forceResolver: Boolean, name: String, omittable: Boolean) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION\n"
	for _, tc := range []struct {
		field string
		cfg   FieldDirective
		want  []string
	}{
		{`n: Int @goField(name: "N2")`, FieldDirective{Name: "goField", ForceResolverArg: "forceResolver"},
			[]string{"T.n", "@goField(name:)", "nameArg"}},
		{`n: Int @goField(omittable: true)`, goFieldNamed,
			[]string{"T.n", "@goField(omittable:)", "nullableInputOmittable"}},
		{`n: Int @goField(forceResolver: true)`, FieldDirective{Name: "goField", NameArg: "name"},
			[]string{"T.n", "@goField(forceResolver:)", "forceResolverArg"}},
	} {
		sdl := decl + "type T { " + tc.field + " }\ntype Query { t: T }\n"
		_, err := generateWith(t, sdl, Config{FieldDirective: tc.cfg})
		if err == nil {
			t.Errorf("%s with %+v was accepted", tc.field, tc.cfg)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error should mention %s, got: %v", tc.field, w, err)
			}
		}
	}
}

// A name that cannot be a Go field is refused before anything is written,
// naming the coordinate, rather than becoming an unexported field Input[T]
// skips.
func TestFieldDirectiveRefusesANameThatIsNotAnExportedIdentifier(t *testing.T) {
	const decl = "directive @goField(name: String) on FIELD_DEFINITION\n"
	for _, bad := range []string{`"lower"`, `""`, `"Two Words"`, `"1X"`, `true`} {
		sdl := decl + "type T { n: Int @goField(name: " + bad + ") }\ntype Query { t: T }\n"
		_, err := generateWith(t, sdl, Config{FieldDirective: goFieldNamed})
		if err == nil || !strings.Contains(err.Error(), "T.n") {
			t.Errorf("name: %s should be refused naming T.n, got %v", bad, err)
		}
	}
}
