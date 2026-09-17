package graphql

import (
	"context"
	"slices"
	"strconv"
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
	// Names are distinctive rather than single letters so that a coordinate
	// match in the assertion below actually pins the right location, rather
	// than merely matching a substring of some other, unrelated identifier.
	cases := []struct {
		name  string
		extra string // appended SDL carrying the misplaced directive
		coord string
	}{
		{"union", `union MisplacedUnion @requiresScopes(scopes: [["x"]]) = UnionMember`, "MisplacedUnion"},
		{"enum", `enum MisplacedEnum @requiresScopes(scopes: [["x"]]) { A }`, "MisplacedEnum"},
		{"enum value", `enum MisplacedEnumValue { A @requiresScopes(scopes: [["x"]]) }`, "MisplacedEnumValue.A"},
		{"scalar", `scalar MisplacedScalar @requiresScopes(scopes: [["x"]])`, "MisplacedScalar"},
		{"input object", `input MisplacedInputObject @requiresScopes(scopes: [["x"]]) { a: String }`, "MisplacedInputObject"},
		{"input field", `input MisplacedInputField { a: String @requiresScopes(scopes: [["x"]]) }`, "MisplacedInputField.a"},
		{"argument", `type MisplacedArgOwner { f(a: String @requiresScopes(scopes: [["x"]])): String }`, "MisplacedArgOwner.f(a:)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE | UNION | ENUM | ENUM_VALUE | SCALAR | INPUT_OBJECT | INPUT_FIELD_DEFINITION | ARGUMENT_DEFINITION
type UnionMember { a: String }
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

// A directive naming it repeatable, or an extension re-declaring it, adds a
// second occurrence to the same Directives list rather than replacing the
// first: gqlparser merges extension directives and skips the non-repeatable
// check for them. requirementOf must AND every occurrence it finds, or the
// second declaration is silently dropped from the effective requirement.
func TestRequirementCombinesEveryOccurrenceOnExtendType(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Dog @requiresScopes(scopes: [["a"]]) { name: String! }
extend type Dog @requiresScopes(scopes: [["b"]])
type Query { dog: Dog! }
`
	s, err := NewSchema(SDL(sdl),
		Query(Field("dog", func(Root) *authzDog { return &authzDog{} })),
		Object[authzDog]("Dog", Field("name", func(*authzDog) string { return "" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	req := s.objects["Dog"].requires
	held := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	if req.Satisfied(held("a")) {
		t.Error("extend type Dog's second @requiresScopes was dropped: satisfied by a alone")
	}
	if !req.Satisfied(held("a", "b")) {
		t.Errorf("Dog's type-level requirement should be a AND b, got %v", req.anyOf)
	}
}

// Same as above for an interface split across a base declaration and an
// extension, which is where the reviewer confirmed the merge behaviour.
func TestRequirementCombinesEveryOccurrenceOnExtendInterface(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
interface Pet @requiresScopes(scopes: [["a"]]) { name: String! }
extend interface Pet @requiresScopes(scopes: [["b"]])
type Dog implements Pet { name: String! }
type Query { pet: Pet! }
`
	s, err := NewSchema(SDL(sdl),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog", Field("name", func(*authzDog) string { return "" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	req := s.objects["Dog"].requires
	held := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	if req.Satisfied(held("a")) {
		t.Error("extend interface Pet's second @requiresScopes was dropped: Dog satisfied by a alone")
	}
	if !req.Satisfied(held("a", "b")) {
		t.Errorf("Dog's inherited type-level requirement should be a AND b, got %v", req.anyOf)
	}
}

// A directive declared repeatable can occur twice on one field without any
// extend at all; the same ForName-only bug drops the second occurrence here
// too.
func TestRequirementCombinesRepeatableDirectiveOccurrencesOnAField(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) repeatable on FIELD_DEFINITION | OBJECT
type Query { a: String! @requiresScopes(scopes: [["x"]]) @requiresScopes(scopes: [["y"]]) }
`
	s, err := NewSchema(SDL(sdl), Query(Field("a", func(Root) string { return "" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	req := s.objects["Query"].fields["a"].requires
	held := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	if req.Satisfied(held("x")) {
		t.Error("the second @requiresScopes occurrence was dropped: satisfied by x alone")
	}
	if !req.Satisfied(held("x", "y")) {
		t.Errorf("Query.a's requirement should be x AND y, got %v", req.anyOf)
	}
}

// checkRequiresScopes must shape-check every occurrence, not only the first
// ForName match, or a malformed second declaration silently passes the build.
func TestNewSchemaRejectsMalformedSecondOccurrence(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) repeatable on FIELD_DEFINITION | OBJECT
type Query { a: String! @requiresScopes(scopes: [["x"]]) @requiresScopes(scopes: ["y"]) }
`
	_, err := NewSchema(SDL(sdl), Query(Field("a", func(Root) string { return "" })))
	if err == nil {
		t.Fatal("NewSchema accepted a malformed second @requiresScopes occurrence")
	}
	if !strings.Contains(err.Error(), "Query.a") {
		t.Errorf("error does not name the coordinate Query.a: %v", err)
	}
}

// A @requiresScopes on the schema definition itself reads as "the whole API
// requires x" while guarding nothing: nothing walks b.ast.SchemaDirectives.
func TestRequiresScopesOnSchemaDefinitionFailsBuild(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on SCHEMA | FIELD_DEFINITION
schema @requiresScopes(scopes: [["x"]]) { query: Query }
type Query { ok: String }
`
	_, err := NewSchema(SDL(sdl), Query(Field("ok", func(Root) string { return "" })))
	if err == nil {
		t.Fatal("NewSchema accepted @requiresScopes on the schema definition")
	}
	if !strings.Contains(err.Error(), "not enforced") {
		t.Errorf("error does not say the schema-level directive is unenforced: %v", err)
	}
}

// Same as above via `extend schema`, which gqlparser folds into the same
// b.ast.SchemaDirectives list as the primary schema block.
func TestRequiresScopesOnExtendSchemaFailsBuild(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on SCHEMA | FIELD_DEFINITION
type Query { ok: String }
extend schema @requiresScopes(scopes: [["x"]])
`
	_, err := NewSchema(SDL(sdl), Query(Field("ok", func(Root) string { return "" })))
	if err == nil {
		t.Fatal("NewSchema accepted @requiresScopes on an extend schema block")
	}
	if !strings.Contains(err.Error(), "not enforced") {
		t.Errorf("error does not say the schema-level directive is unenforced: %v", err)
	}
}

// A directive definition's own argument is a type-system position nothing
// enforces either: b.ast.Directives[name].Arguments is never walked.
func TestRequiresScopesOnADirectiveDefinitionArgumentFailsBuild(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | ARGUMENT_DEFINITION
directive @foo(a: String @requiresScopes(scopes: [["x"]])) on FIELD_DEFINITION
type Query { ok: String }
`
	_, err := NewSchema(SDL(sdl), Query(Field("ok", func(Root) string { return "" })))
	if err == nil {
		t.Fatal("NewSchema accepted @requiresScopes on a directive definition's argument")
	}
	if !strings.Contains(err.Error(), "@foo(a:)") || !strings.Contains(err.Error(), "not enforced") {
		t.Errorf("error should name @foo(a:) and say it is not enforced: %v", err)
	}
}

// And allocates len(acc)*len(next) groups; combining several interfaces'
// many-group requirements can multiply into an enormous allocation before a
// check made only after the fact ever runs. The fix must check the product
// before each And, fail once with the type's own coordinate, and skip the
// object's fields entirely rather than have each of them redo the same
// explosion and report its own error.
func TestEffectiveRequirementCapCheckedBeforeCombining(t *testing.T) {
	manyGroups := func(prefix string, n int) string {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(`["` + prefix + strconv.Itoa(i) + `"]`)
		}
		b.WriteString("]")
		return b.String()
	}
	sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
interface I1 @requiresScopes(scopes: ` + manyGroups("a", 30) + `) { f: String! g: String! }
interface I2 @requiresScopes(scopes: ` + manyGroups("b", 30) + `) { f: String! g: String! }
type Big implements I1 & I2 { f: String! g: String! }
type Query { big: Big! }
`
	_, err := NewSchema(SDL(sdl),
		Query(Field("big", func(Root) *authzFoo { return &authzFoo{} })),
		Object[authzFoo]("Big",
			Field("f", func(*authzFoo) string { return "" }),
			Field("g", func(*authzFoo) string { return "" }),
		),
	)
	if err == nil {
		t.Fatal("NewSchema accepted an effective requirement whose product is astronomically large")
	}
	if !strings.Contains(err.Error(), "Big") {
		t.Errorf("error does not name the type Big: %v", err)
	}
	if n := strings.Count(err.Error(), "@requiresScopes"); n != 1 {
		t.Errorf("got %d cap-related errors, want exactly 1 (per-field errors must be suppressed once the type-level value already failed): %v", n, err)
	}
}

// `type Dog implements Pet` plus `extend type Dog implements Pet` gives
// obj.def.Interfaces = [Pet, Pet]; ANDing Pet's requirement with itself
// squares its own group count and can trip the cap on a schema that would
// otherwise be well within it.
func TestDuplicateInterfaceNameIsNotCombinedTwice(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
interface Pet @requiresScopes(scopes: [["a"], ["b"], ["c"], ["d"], ["e"], ["f"], ["g"], ["h"]]) { name: String! }
type Dog implements Pet { name: String! }
extend type Dog implements Pet
type Query { pet: Pet! }
`
	s, err := NewSchema(SDL(sdl),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog", Field("name", func(*authzDog) string { return "" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v (Pet's 8 groups combined with itself would be 64, exactly at the cap, but combined with anything else would trip it)", err)
	}
	dog := s.objects["Dog"]
	if got := dog.requires.groupCount(); got != 8 {
		t.Errorf("Dog's type-level requirement has %d groups, want 8 (Pet counted once, not squared to 64)", got)
	}
}

// gqlparser accepts repeated `extend type T @requiresScopes(...)` even when
// the directive is not declared repeatable, and requirementOf must AND every
// occurrence (fix round 1) -- but combining them is itself where the
// resource cost was: four occurrences of 30 groups multiply to 810,000
// before any caller-level check (resolveAuthRequirements' post-hoc
// groupCount comparison) ever got a chance to run, all inside requirementOf
// with no cap of its own (fix round 2). Correctness held even before round
// 2 -- this exact scenario already failed with one error under the
// unmodified round-1 code, just after paying for the full 810,000-group
// allocation first; see the task-2 fix-round-2 report for the measured
// before/after cost of that allocation. This pins the correctness outcome
// permanently; andCapped inside requirementOf is what makes it cheap.
func TestExtendOccurrencesOverTheCapFailBuildWithOneError(t *testing.T) {
	manyGroups := func(prefix string, n int) string {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(`["` + prefix + strconv.Itoa(i) + `"]`)
		}
		b.WriteString("]")
		return b.String()
	}
	sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Big @requiresScopes(scopes: ` + manyGroups("a", 30) + `) { f: String! }
extend type Big @requiresScopes(scopes: ` + manyGroups("b", 30) + `)
extend type Big @requiresScopes(scopes: ` + manyGroups("c", 30) + `)
extend type Big @requiresScopes(scopes: ` + manyGroups("d", 30) + `)
type Query { big: Big! }
`
	_, err := NewSchema(SDL(sdl),
		Query(Field("big", func(Root) *authzFoo { return &authzFoo{} })),
		Object[authzFoo]("Big", Field("f", func(*authzFoo) string { return "" })),
	)
	if err == nil {
		t.Fatal("NewSchema accepted a type whose extend occurrences multiply to 810,000 groups")
	}
	if !strings.Contains(err.Error(), "Big") {
		t.Errorf("error does not name the type Big: %v", err)
	}
	if n := strings.Count(err.Error(), "@requiresScopes"); n != 1 {
		t.Errorf("got %d cap-related errors, want exactly 1: %v", n, err)
	}
}

func inheritExec(t testing.TB, held ...string) *Executor {
	t.Helper()
	have := map[string]bool{}
	for _, h := range held {
		have[h] = true
	}
	return NewExecutor(inheritSchema(t), WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return have })))
}

func TestInheritedRequirementDeniesField(t *testing.T) {
	cases := []struct {
		name  string
		held  []string
		query string
		deny  bool
	}{
		{"object-level denies a field that declares nothing", []string{"pet:read"}, `{ pet { name } }`, true},
		{"interface type-level denies", []string{"dog:read"}, `{ pet { name } }`, true},
		{"interface field-level denies the implementer's field", []string{"dog:read", "pet:read"}, `{ pet { secret } }`, true},
		{"same, selected through an inline fragment", []string{"dog:read", "pet:read"}, `{ pet { ... on Dog { secret } } }`, true},
		{"everything held allows", []string{"dog:read", "pet:read", "pet:secret"}, `{ pet { name secret } }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, inheritExec(t, tc.held...), tc.query, "")
			if !tc.deny {
				if len(resp.Errors) > 0 {
					t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
				}
				return
			}
			if len(resp.Errors) == 0 {
				t.Fatalf("not denied; data = %s", resp.Data)
			}
			if got := resp.Errors[0].Extensions["code"]; got != CodeForbidden {
				t.Errorf("code = %v, want %v", got, CodeForbidden)
			}
		})
	}
}

// Apollo clients add __typename to every selection. Unguarded, it counts the
// rows of a guarded type and confirms a given one exists.
func TestTypenameIsGuardedByTheObjectRequirement(t *testing.T) {
	resp := run(t, inheritExec(t), `{ pet { __typename } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatalf("__typename of a guarded type was not denied; data = %s", resp.Data)
	}
	if string(resp.Data) != "null" && resp.Data != nil {
		t.Errorf("__typename is String!, so its denial must bubble; data = %s", resp.Data)
	}

	resp = run(t, inheritExec(t, "dog:read", "pet:read"), `{ pet { __typename } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("held scopes still denied __typename: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"pet":{"__typename":"Dog"}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

func TestTypenameOfAnUnguardedTypeHasNoSite(t *testing.T) {
	e := NewExecutor(shapeSchema(t))
	p, _, perrs := planForTest(t, e, `{ me { __typename id } }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	for _, s := range p.shape.Sites() {
		if s.Kind == SiteObject {
			t.Errorf("unguarded type User produced an object site %q", s.Coord)
		}
	}
}

func TestObjectSiteAdmitsOnlyAllowAndDeny(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Outcome
	}{
		{"Null", Null()},
		{"Zero", Zero()},
		{"Redact", Redact(func(v any) any { return v })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var setErr error
			e := NewExecutor(inheritSchema(t), WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					for i, s := range shape.Sites() {
						if s.Kind == SiteObject {
							setErr = d.Set(i, tc.o)
						}
					}
					return nil
				})))
			run(t, e, `{ pet { __typename } }`, "")
			if setErr == nil {
				t.Errorf("Decision.Set accepted %s on an object site", tc.name)
			}
		})
	}
}
