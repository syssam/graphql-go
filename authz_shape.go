package graphql

import (
	"slices"

	"github.com/vektah/gqlparser/v2/ast"
)

// authDirective is the SDL directive read into a Requirement. It is a
// package-level name rather than an option because the shape must be
// identical for every executor sharing a schema: the shape travels with the
// cached plan, and a per-executor reading would make one executor's plan
// wrong for another's.
const authDirective = "requiresScopes"

// buildAuthShape walks a compiled selection and records every position that
// declares a requirement. It runs once per plan, so the walk is O(plan size)
// and never repeated per request.
func buildAuthShape(sel *selectionSet) *AuthShape {
	b := &shapeBuilder{}
	b.walk(sel)
	if len(b.sites) == 0 {
		return nil
	}
	slices.Sort(b.scopes)
	return &AuthShape{sites: b.sites, scopes: slices.Compact(b.scopes)}
}

type shapeBuilder struct {
	sites  []AuthSite
	scopes []string
	seen   map[*selectionSet]bool
}

func (b *shapeBuilder) walk(sel *selectionSet) {
	if sel == nil {
		return
	}
	// A plan for a recursive selection can reach the same set twice; without
	// this the walk would index the same field more than once.
	if b.seen == nil {
		b.seen = make(map[*selectionSet]bool)
	}
	if b.seen[sel] {
		return
	}
	b.seen[sel] = true

	for _, f := range sel.fields {
		b.field(f)
	}
	for _, concrete := range sel.byType {
		b.walk(concrete)
	}
}

func (b *shapeBuilder) field(f *planField) {
	f.authIdx = -1
	if f.def != nil && f.def.def != nil {
		if req, ok := requirementOf(f.def.def.Directives); ok {
			f.authIdx = int32(len(b.sites))
			b.sites = append(b.sites, AuthSite{
				Coord:    coordinate(f.def.object.name, f.name),
				Field:    f.def.def,
				Object:   f.def.object.def,
				Kind:     SiteOutput,
				Requires: req,
			})
			b.scopes = append(b.scopes, req.Scopes()...)
		}
	}
	b.walk(f.sub)
}

// requirementOf reads @requiresScopes off a definition. The argument is a
// list of lists of strings; anything else is a schema error already caught
// at NewSchema, so a malformed value here is treated as no requirement
// rather than as a lockout nobody can diagnose at request time.
func requirementOf(ds ast.DirectiveList) (Requirement, bool) {
	d := ds.ForName(authDirective)
	if d == nil {
		return Requirement{}, false
	}
	arg := d.Arguments.ForName("scopes")
	if arg == nil || arg.Value == nil {
		return Requirement{}, false
	}
	var groups [][]string
	for _, outer := range arg.Value.Children {
		var group []string
		for _, inner := range outer.Value.Children {
			group = append(group, inner.Value.Raw)
		}
		groups = append(groups, group)
	}
	if groups == nil {
		return Requirement{}, false
	}
	return Requirement{anyOf: groups}, true
}
