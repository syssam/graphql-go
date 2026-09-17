package graphql

import (
	"slices"
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
