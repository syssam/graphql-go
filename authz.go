package graphql

import (
	"slices"

	"github.com/vektah/gqlparser/v2/ast"
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
// Each group is cloned to prevent external mutations from changing the requirement
// after construction, which is important for Requirement's use on the concurrent
// request path and its immutability contract everywhere else.
func NewRequirement(anyOf ...[]string) Requirement {
	cloned := make([][]string, len(anyOf))
	for i, group := range anyOf {
		cloned[i] = slices.Clone(group)
	}
	return Requirement{anyOf: cloned}
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

// SiteKind says what kind of position needs an authorization decision.
type SiteKind uint8

const (
	// SiteOutput is a selected output field.
	SiteOutput SiteKind = iota
	// SiteObject is a whole object type, checked once per instance rather
	// than once per field of it.
	SiteObject
	// SiteFilterArg is a coordinate named in a where, orderBy or groupBy
	// argument. A restricted field must not be filterable either, or its
	// value leaks by bisection without ever being selected.
	SiteFilterArg
	// SiteInputWrite is a coordinate written by a mutation input.
	SiteInputWrite
)

// AuthSite is one position in a plan that may need a decision.
type AuthSite struct {
	Coord    string
	Field    *ast.FieldDefinition // nil when Kind is SiteObject
	Object   *ast.Definition
	Kind     SiteKind
	Requires Requirement
	Grants   []string
}

// AuthShape is what an operation touches, independent of who is asking. It
// is computed once per compiled plan and cached with it, which is why the
// plan cache is not multiplied by the number of distinct policies.
type AuthShape struct {
	sites  []AuthSite
	scopes []string
}

// Sites returns the positions needing a decision, indexed by site index.
// planField.authIdx indexes into this slice.
func (s *AuthShape) Sites() []AuthSite {
	if s == nil {
		return nil
	}
	return s.sites
}

// Scopes returns every scope named anywhere in the operation, sorted and
// deduplicated, so an Authorizer can load them in one round trip.
func (s *AuthShape) Scopes() []string {
	if s == nil {
		return nil
	}
	return s.scopes
}

// IsEmpty reports whether the operation touches nothing that declares a
// requirement. An Authorizer is not consulted for such an operation.
func (s *AuthShape) IsEmpty() bool { return s == nil || len(s.sites) == 0 }
