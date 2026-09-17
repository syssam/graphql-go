package graphql

import (
	"slices"
	"strings"
	"testing"
)

func TestRequirementAnd(t *testing.T) {
	held := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	cases := []struct {
		name string
		got  Requirement
		want [][]string
	}{
		{"zero and x is x", Requirement{}.And(NewRequirement([]string{"a"})), [][]string{{"a"}}},
		{"x and zero is x", NewRequirement([]string{"a"}).And(Requirement{}), [][]string{{"a"}}},
		{"single groups merge", NewRequirement([]string{"a"}).And(NewRequirement([]string{"b"})), [][]string{{"a", "b"}}},
		{"cross product", NewRequirement([]string{"a"}, []string{"b"}).And(NewRequirement([]string{"c"})), [][]string{{"a", "c"}, {"b", "c"}}},
		{"duplicate scopes collapse", NewRequirement([]string{"a"}).And(NewRequirement([]string{"a"})), [][]string{{"a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !slices.EqualFunc(tc.got.anyOf, tc.want, slices.Equal[[]string]) {
				t.Errorf("anyOf = %v, want %v", tc.got.anyOf, tc.want)
			}
		})
	}

	// Semantics, not just shape: an AND must demand both sides.
	r := NewRequirement([]string{"a"}, []string{"b"}).And(NewRequirement([]string{"c"}))
	if r.Satisfied(held("a")) {
		t.Error("(a|b)&c satisfied by a alone")
	}
	if !r.Satisfied(held("b", "c")) {
		t.Error("(a|b)&c not satisfied by b,c")
	}
}

func TestRequirementAndDoesNotAliasItsInputs(t *testing.T) {
	a := NewRequirement([]string{"a"})
	b := NewRequirement([]string{"b"})
	r := a.And(b)
	r.anyOf[0][0] = "mutated"
	if a.anyOf[0][0] != "a" || b.anyOf[0][0] != "b" {
		t.Error("And shares backing arrays with its operands")
	}
}

const inheritSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
interface Pet @requiresScopes(scopes: [["pet:read"]]) {
  name: String!
  secret: String! @requiresScopes(scopes: [["pet:secret"]])
}
type Dog implements Pet @requiresScopes(scopes: [["dog:read"]]) {
  name: String!
  secret: String!
  bark: String! @requiresScopes(scopes: [["dog:bark"]])
}
type Query { pet: Pet! }
`

func inheritSchema(t testing.TB) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(inheritSDL),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog",
			Field("name", func(*authzDog) string { return "rex" }),
			Field("secret", func(*authzDog) string { return "bones" }),
			Field("bark", func(*authzDog) string { return "woof" }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}

func TestEffectiveRequirementsAreComputedAtBuild(t *testing.T) {
	s := inheritSchema(t)
	dog := s.objects["Dog"]
	all := func(scopes ...string) map[string]bool {
		m := map[string]bool{}
		for _, sc := range scopes {
			m[sc] = true
		}
		return m
	}

	if dog.requires.Satisfied(all("dog:read")) {
		t.Error("Dog's type-level requirement ignores its interface's type-level requirement")
	}
	if !dog.requires.Satisfied(all("dog:read", "pet:read")) {
		t.Error("Dog's type-level requirement is not dog:read AND pet:read")
	}

	name := dog.fields["name"].requires
	if !name.Satisfied(all("dog:read", "pet:read")) || name.Satisfied(all("dog:read")) {
		t.Errorf("Dog.name should inherit exactly dog:read AND pet:read, got %v", name.anyOf)
	}

	secret := dog.fields["secret"].requires
	if secret.Satisfied(all("dog:read", "pet:read")) {
		t.Error("Dog.secret does not inherit Pet.secret's field-level requirement")
	}
	if !secret.Satisfied(all("dog:read", "pet:read", "pet:secret")) {
		t.Errorf("Dog.secret should need dog:read, pet:read and pet:secret, got %v", secret.anyOf)
	}

	bark := dog.fields["bark"].requires
	if !bark.Satisfied(all("dog:read", "pet:read", "dog:bark")) || bark.Satisfied(all("dog:read", "pet:read")) {
		t.Errorf("Dog.bark should add its own dog:bark, got %v", bark.anyOf)
	}

	if q := s.objects["Query"].fields["pet"].requires; !q.IsZero() {
		t.Errorf("Query.pet declares nothing and inherits nothing, got %v", q.anyOf)
	}
}

func TestEffectiveRequirementOverTheGroupCapFailsBuild(t *testing.T) {
	// 9 groups on the object times 9 on the field is 81, over the cap of 64.
	nine := func(prefix string) string {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < 9; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(`["` + prefix + string(rune('a'+i)) + `"]`)
		}
		b.WriteString("]")
		return b.String()
	}
	sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Big @requiresScopes(scopes: ` + nine("o") + `) { f: String! @requiresScopes(scopes: ` + nine("f") + `) }
type Query { big: Big! }
`
	_, err := NewSchema(SDL(sdl),
		Query(Field("big", func(Root) *authzFoo { return &authzFoo{} })),
		Object[authzFoo]("Big", Field("f", func(*authzFoo) string { return "" })),
	)
	if err == nil {
		t.Fatal("NewSchema accepted an effective requirement of 81 groups")
	}
	if !strings.Contains(err.Error(), "Big.f") {
		t.Errorf("error does not name the coordinate: %v", err)
	}
}

func TestRequiresScopesOnAnUnenforcedLocationFailsBuild(t *testing.T) {
	cases := []struct {
		name  string
		extra string // appended SDL carrying the misplaced directive
		coord string
	}{
		{"union", `union U @requiresScopes(scopes: [["x"]]) = Q2`, "U"},
		{"enum", `enum E @requiresScopes(scopes: [["x"]]) { A }`, "E"},
		{"enum value", `enum E2 { A @requiresScopes(scopes: [["x"]]) }`, "E2.A"},
		{"scalar", `scalar S @requiresScopes(scopes: [["x"]])`, "S"},
		{"input object", `input I @requiresScopes(scopes: [["x"]]) { a: String }`, "I"},
		{"input field", `input J { a: String @requiresScopes(scopes: [["x"]]) }`, "J.a"},
		{"argument", `type Q3 { f(a: String @requiresScopes(scopes: [["x"]])): String }`, "Q3.f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE | UNION | ENUM | ENUM_VALUE | SCALAR | INPUT_OBJECT | INPUT_FIELD_DEFINITION | ARGUMENT_DEFINITION
type Q2 { a: String }
type Query { ok: String }
` + tc.extra
			_, err := NewSchema(SDL(sdl), Query(Field("ok", func(Root) string { return "" })))
			if err == nil {
				t.Fatalf("NewSchema accepted @requiresScopes on a %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.coord) || !strings.Contains(err.Error(), "not enforced") {
				t.Errorf("error should name %q and say it is not enforced: %v", tc.coord, err)
			}
		})
	}
}
