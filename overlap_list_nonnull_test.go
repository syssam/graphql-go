package graphql

import (
	"fmt"
	"strings"
	"testing"
)

type overlapU struct{}

// Above 256 selections gqlparser's merge rule is replaced by a linear one,
// which must agree with it on validity. gqlparser recurses into a list without
// comparing the list's own nullability; the replacement compared it, so
// `x: [Int]!` against `x: [Int]` under one response name was valid in a small
// document and a validation error once unrelated fields were added.
func TestMergeValidityDoesNotDependOnDocumentSize(t *testing.T) {
	sdl := `
		type A { x: [Int]!  z: [[Int]!] }
		type B { x: [Int]   z: [[Int]] }
		union U = A | B
		type Query { u: U n: Int }`
	s, err := NewSchema(SDL(sdl),
		Union[any]("U"),
		Object[overlapU]("A", Field("x", func(*overlapU) []*int { return []*int{} }), Field("z", func(*overlapU) [][]*int { return nil })),
		Object[struct{ _ int }]("B", Field("x", func(*struct{ _ int }) []*int { return nil }), Field("z", func(*struct{ _ int }) [][]*int { return nil })),
		Query(Field("u", func(Root) any { return &overlapU{} }), Field("n", func(Root) *int { return nil })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	for _, sel := range []string{`u { ... on A { x } ... on B { x } }`, `u { ... on A { z } ... on B { z } }`} {
		small := run(t, e, `{ `+sel+` }`, "")
		var b strings.Builder
		b.WriteString("{ " + sel)
		for i := range 260 {
			fmt.Fprintf(&b, " n%d: n", i)
		}
		b.WriteString(" }")
		large := run(t, e, b.String(), "")
		if (len(small.Errors) == 0) != (len(large.Errors) == 0) {
			t.Errorf("%s: small document errors=%s, the same selection with 260 unrelated fields errors=%s",
				sel, errorsJSON(small.Errors), errorsJSON(large.Errors))
		}
	}
}
