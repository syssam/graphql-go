package graphql

import (
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
