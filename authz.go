package graphql

import (
	"slices"
)

// Requirement is an OR of ANDs: satisfied when every scope in any one group
// is held. The zero Requirement is satisfied by everyone, so a field that
// declares nothing is not accidentally locked.
//
// The shape mirrors Apollo's @requiresScopes(scopes: [[String!]!]!), which is
// the vocabulary most tooling already reads.
type Requirement struct {
	anyOf [][]string
}

// NewRequirement builds a Requirement from its groups.
func NewRequirement(anyOf ...[]string) Requirement {
	return Requirement{anyOf: anyOf}
}

// IsZero reports whether the requirement admits everyone.
func (r Requirement) IsZero() bool { return len(r.anyOf) == 0 }

// Satisfied reports whether held covers any one of the groups.
func (r Requirement) Satisfied(held map[string]bool) bool {
	if r.IsZero() {
		return true
	}
	for _, group := range r.anyOf {
		ok := true
		for _, scope := range group {
			if !held[scope] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Scopes returns every scope the requirement names, sorted and deduplicated,
// so a caller can load them from a policy source in one round trip.
func (r Requirement) Scopes() []string {
	var out []string
	for _, group := range r.anyOf {
		out = append(out, group...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
