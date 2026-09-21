package graphql

import (
	"strings"
	"testing"
)

// FuzzRequirementDirectiveLiteral drives NewSchema with arbitrary directive
// argument literals. requirementOf reads them by walking the AST -- elem.Value.Raw
// for a flat shape, outer.Value.Children for a nested one -- and trusts
// checkRequirementDirectives to have rejected anything the shape cannot hold.
// That trust is misplaced by construction: build() accumulates errors and keeps
// going, so the reader runs over a literal already known to be malformed. A
// panic there is a crash at NewSchema, which no request-path fuzzer reaches
// because it never gets that far.
func FuzzRequirementDirectiveLiteral(f *testing.F) {
	for _, seed := range []string{
		`[["a"]]`, `["a"]`, `["a","b"]`, `[["a","b"],["c"]]`,
		`["flat"]`, `[[1,2]]`, `[[null]]`, `{x: 1}`, `[[]]`, `[]`,
		`[SOME_ENUM]`, `[{x: 1}]`, `"admin"`, `null`, `1`, `true`,
		`[[["deep"]]]`, `[""]`, `[[""]]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, lit string) {
		// A literal containing a closing brace or a directive can escape the
		// template and make this a test of gqlparser's parser rather than of
		// the reader; skip those rather than chase parse errors.
		if strings.ContainsAny(lit, "}\x00") || strings.Contains(lit, "@") {
			t.Skip()
		}
		for _, shape := range []ScopeShape{ScopesNested, ScopesAllOf, ScopesAnyOf} {
			sdl := "directive @auth(requires: [String!]) on FIELD_DEFINITION\n" +
				"type Query { a: String! @auth(requires: " + lit + ") }"
			s, err := NewSchema(SDL(sdl),
				RequirementDirective("auth", "requires", shape),
				Query(Field("a", func(Root) string { return "" })))
			if err != nil {
				continue
			}
			// A schema that built must carry a self-consistent requirement:
			// no group may be empty, since Satisfied over an empty group is
			// vacuously true and would guard nothing while reading as guarded.
			for _, g := range s.objects["Query"].fields["a"].requires.anyOf {
				if len(g) == 0 {
					t.Fatalf("shape %d, literal %q: built a requirement with an empty group, which nothing can fail", shape, lit)
				}
			}
		}
	})
}
