package graphql

import (
	"context"
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
