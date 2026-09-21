package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const legacySDL = `
directive @auth(requires: [String!]) on OBJECT | FIELD_DEFINITION
type Query { secret: String! @auth(requires: ["admin"]) }
`

// A bad declaration must fail the build. Each of these is a way to configure a
// directive that would otherwise read as "no requirement" on every site while
// the schema builds clean.
func TestRequirementDirectiveDeclarationErrors(t *testing.T) {
	cases := []struct {
		name    string
		opt     SchemaOption
		wantErr string
	}{
		{"undeclared shape", RequirementDirective("auth", "requires", 0), "shape must be declared"},
		{"shape out of range", RequirementDirective("auth", "requires", ScopeShape(99)), "shape must be declared"},
		{"directive not in the SDL", RequirementDirective("nosuch", "requires", ScopesAllOf), "no directive @nosuch"},
		{"argument the directive lacks", RequirementDirective("auth", "scope", ScopesAllOf), `has no argument "scope"`},
		{"redeclaring the built-in", RequirementDirective("requiresScopes", "scopes", ScopesAllOf), "already declared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(legacySDL), c.opt,
				Query(Field("secret", func(Root) string { return "s" })))
			if err == nil {
				t.Fatal("declaration was accepted")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// Declaring one custom name twice is the same hazard as redeclaring the
// built-in: two option calls silently racing to be last.
func TestRequirementDirectiveRejectsADuplicateName(t *testing.T) {
	_, err := NewSchema(SDL(legacySDL),
		RequirementDirective("auth", "requires", ScopesAllOf),
		RequirementDirective("auth", "requires", ScopesAnyOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil || !strings.Contains(err.Error(), "already declared") {
		t.Fatalf("error = %v, want a duplicate-name rejection", err)
	}
}

// The control: without it, every test above could pass for the wrong reason if
// NewSchema rejected the fixture outright.
func TestRequirementDirectiveValidDeclarationBuilds(t *testing.T) {
	if _, err := NewSchema(SDL(legacySDL),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" }))); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
}

// scopesOf renders a coordinate's effective requirement as its groups, so a
// test asserts the composition rather than a boolean.
func scopesOf(t *testing.T, s *Schema, typeName, field string) [][]string {
	t.Helper()
	obj := s.objects[typeName]
	if obj == nil {
		t.Fatalf("no object %q", typeName)
	}
	fd := obj.fields[field]
	if fd == nil {
		t.Fatalf("no field %s.%s", typeName, field)
	}
	return fd.requires.anyOf
}

func sameGroups(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				return false
			}
		}
	}
	return true
}

// The same SDL must read differently under each shape. This is the assertion
// the design turns on: the engine cannot infer AND vs OR, so it must honour
// what was declared.
func TestScopeShapeChangesTheRequirement(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on FIELD_DEFINITION
type Query { secret: String! @auth(requires: ["a","b"]) }
`
	for _, c := range []struct {
		shape ScopeShape
		want  [][]string
	}{
		{ScopesAllOf, [][]string{{"a", "b"}}},
		{ScopesAnyOf, [][]string{{"a"}, {"b"}}},
	} {
		s, err := NewSchema(SDL(sdl),
			RequirementDirective("auth", "requires", c.shape),
			Query(Field("secret", func(Root) string { return "s" })))
		if err != nil {
			t.Fatalf("shape %d: NewSchema: %v", c.shape, err)
		}
		if got := scopesOf(t, s, "Query", "secret"); !sameGroups(got, c.want) {
			t.Fatalf("shape %d: groups = %v, want %v", c.shape, got, c.want)
		}
	}
}

// Both spellings on one coordinate compose the way repeated occurrences of one
// directive already do: ANDed. More declarations never mean less restrictive.
func TestBothSpellingsOnOneFieldAnd(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on FIELD_DEFINITION
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION
type Query { secret: String! @auth(requires: ["a"]) @requiresScopes(scopes: [["b"]]) }
`
	s, err := NewSchema(SDL(sdl),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	got := scopesOf(t, s, "Query", "secret")
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("groups = %v, want one group of two (a AND b)", got)
	}
}

// End to end: a custom directive must be enforced, not merely recorded.
func TestCustomDirectiveIsEnforced(t *testing.T) {
	s, err := NewSchema(SDL(legacySDL),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	held := map[string]bool{}
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return held })))
	resp := run(t, e, `{ secret }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a guarded field resolved without the scope")
	}
	held["admin"] = true
	if resp = run(t, e, `{ secret }`, ""); len(resp.Errors) != 0 {
		t.Fatalf("holding the scope still denied: %v", resp.Errors)
	}
}

// gqlparser checks a directive's name, location and argument presence, never
// its argument literal. Without a per-shape check, @auth(requires: "admin") --
// a string where a list belongs -- builds clean and guards nothing.
func TestCustomDirectiveMalformedLiteralIsRejected(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on FIELD_DEFINITION
type Query { secret: String! @auth(requires: "admin") }
`
	_, err := NewSchema(SDL(sdl),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil {
		t.Fatal("a malformed literal was accepted")
	}
	if !strings.Contains(err.Error(), "@auth") {
		t.Fatalf("error does not name the offending directive: %v", err)
	}
}

// A custom directive in a position the engine does not enforce must be a build
// error naming that directive, exactly as @requiresScopes is. Reading a
// directive without also rejecting its misplacement is the fail-open this
// design exists to prevent.
func TestCustomDirectiveMisplacementIsRejected(t *testing.T) {
	for _, c := range []struct{ name, sdl string }{
		{"input field", `
directive @auth(requires: [String!]) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
input Where { name: String @auth(requires: ["admin"]) }
type Query { secret(w: Where): String! }
`},
		{"field argument", `
directive @auth(requires: [String!]) on FIELD_DEFINITION | ARGUMENT_DEFINITION
type Query { secret(id: ID! @auth(requires: ["admin"])): String! }
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(c.sdl),
				RequirementDirective("auth", "requires", ScopesAllOf))
			if err == nil {
				t.Fatal("a misplaced custom directive was accepted")
			}
			if !strings.Contains(err.Error(), "@auth") {
				t.Fatalf("error does not name the offending directive: %v", err)
			}
		})
	}
}

// The group cap protects plan compile from a combinatorial requirement, and
// it must apply to every spelling. AND multiplies group counts, so each
// occurrence has to contribute more than one group to overflow it: a
// two-scope ScopesAnyOf list is two groups, and k of them are 2^k.
func TestCustomDirectiveIsCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("directive @auth(requires: [String!]) repeatable on FIELD_DEFINITION\ntype Query {\n  secret: String!")
	for i := range 16 {
		fmt.Fprintf(&sb, " @auth(requires: [\"a%d\", \"b%d\"])", i, i)
	}
	sb.WriteString("\n}\n")
	_, err := NewSchema(SDL(sb.String()),
		RequirementDirective("auth", "requires", ScopesAnyOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil {
		t.Fatal("an over-cap requirement was accepted")
	}
	if !strings.Contains(err.Error(), "@auth") {
		t.Fatalf("cap error does not name the offending directive: %v", err)
	}
}

type authDirDoc struct {
	ID   string
	Body string
}

// A custom directive must inherit exactly as @requiresScopes does: a field's
// own requirement ANDed with its object type's and each implemented
// interface's. resolveAuthRequirements owns that logic and must not have been
// bypassed by the new reading path.
func TestCustomDirectiveInherits(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on OBJECT | INTERFACE | FIELD_DEFINITION
interface Node @auth(requires: ["node"]) { id: ID! }
type Doc implements Node @auth(requires: ["doc"]) {
  id: ID!
  body: String! @auth(requires: ["body"])
}
type Query { doc: Doc! }
`
	s, err := NewSchema(SDL(sdl),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Object[authDirDoc]("Doc",
			Field("id", func(d *authDirDoc) ID { return ID(d.ID) }),
			Field("body", func(d *authDirDoc) string { return d.Body }),
		),
		Query(Field("doc", func(Root) authDirDoc { return authDirDoc{ID: "1"} })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	got := scopesOf(t, s, "Doc", "body")
	if len(got) != 1 {
		t.Fatalf("groups = %v, want a single ANDed group", got)
	}
	has := map[string]bool{}
	for _, sc := range got[0] {
		has[sc] = true
	}
	for _, want := range []string{"body", "doc", "node"} {
		if !has[want] {
			t.Fatalf("effective requirement %v is missing %q", got, want)
		}
	}
}

// With no option supplied, the default entry must behave exactly as the
// constant did. This is the regression guard for every existing schema.
func TestDefaultSpellingUnchanged(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION
type Query { secret: String! @requiresScopes(scopes: [["a","b"],["c"]]) }
`
	s, err := NewSchema(SDL(sdl), Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	if got := scopesOf(t, s, "Query", "secret"); !sameGroups(got, [][]string{{"a", "b"}, {"c"}}) {
		t.Fatalf("groups = %v, want [[a b] [c]]", got)
	}
}

// @authenticated is Apollo's marker for "any authenticated principal". It
// takes no argument, so no ScopeShape reading an argument can express it.
// Its presence alone is the requirement.
func TestMarkerDirectiveIsEnforced(t *testing.T) {
	const sdl = `
directive @authenticated on OBJECT | FIELD_DEFINITION
type Query { secret: String! @authenticated  open: String! }
`
	s, err := NewSchema(SDL(sdl),
		MarkerDirective("authenticated", "authn"),
		Query(
			Field("secret", func(Root) string { return "s" }),
			Field("open", func(Root) string { return "o" }),
		))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	if got := scopesOf(t, s, "Query", "secret"); !sameGroups(got, [][]string{{"authn"}}) {
		t.Fatalf("groups = %v, want [[authn]]", got)
	}
	if got := scopesOf(t, s, "Query", "open"); len(got) != 0 {
		t.Fatalf("an unmarked field carries %v, want nothing", got)
	}

	held := map[string]bool{}
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return held })))
	if resp := run(t, e, `{ secret }`, ""); len(resp.Errors) == 0 {
		t.Fatal("a marked field resolved for an unauthenticated principal")
	}
	if resp := run(t, e, `{ open }`, ""); len(resp.Errors) != 0 {
		t.Fatalf("an unmarked field was denied: %v", resp.Errors)
	}
	held["authn"] = true
	if resp := run(t, e, `{ secret }`, ""); len(resp.Errors) != 0 {
		t.Fatalf("denied while holding the scope: %v", resp.Errors)
	}
}

// A marker composes with a scope directive the ordinary way: both must be
// satisfied, which is what lets @authenticated sit alongside @requiresScopes
// as Apollo uses them.
func TestMarkerAndsWithAScopeDirective(t *testing.T) {
	const sdl = `
directive @authenticated on FIELD_DEFINITION
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION
type Query { secret: String! @authenticated @requiresScopes(scopes: [["pay"]]) }
`
	s, err := NewSchema(SDL(sdl),
		MarkerDirective("authenticated", "authn"),
		Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	got := scopesOf(t, s, "Query", "secret")
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("groups = %v, want one group of two (authn AND pay)", got)
	}
}

// A marker gets the same safety nets as every other spelling: a position the
// engine does not enforce is a build error naming it.
func TestMarkerMisplacementIsRejected(t *testing.T) {
	const sdl = `
directive @authenticated on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
input Where { name: String @authenticated }
type Query { secret(w: Where): String! }
`
	_, err := NewSchema(SDL(sdl), MarkerDirective("authenticated", "authn"))
	if err == nil {
		t.Fatal("a misplaced marker was accepted")
	}
	if !strings.Contains(err.Error(), "@authenticated") {
		t.Fatalf("error does not name the marker: %v", err)
	}
}

// A marker must declare the scope it stands for; an empty one would make
// every marked field carry a requirement nothing can hold.
func TestMarkerRequiresAScope(t *testing.T) {
	const sdl = `
directive @authenticated on FIELD_DEFINITION
type Query { secret: String! @authenticated }
`
	_, err := NewSchema(SDL(sdl), MarkerDirective("authenticated", ""),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("error = %v, want a rejection naming the missing scope", err)
	}
}
