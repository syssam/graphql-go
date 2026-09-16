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
	// Insurance against a plan that reaches the same *selectionSet twice: this
	// engine's compileSelection allocates a fresh selectionSet per call, so no
	// double-reach has ever been observed, but indexing a field twice would be
	// silent (the second index would win) and the check costs nothing when it
	// never fires.
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

// requirementOf reads @requiresScopes off a definition. NewSchema's
// validateAuthDirectives rejects any use of the directive whose "scopes"
// value is not a non-empty list of non-empty lists of strings, so by the
// time a plan compiles every matched directive's groups are already
// well-formed; this function only extracts them. The nil/absent-argument
// branches below stay as defensive fallbacks, not as the shape guarantee.
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
	return NewRequirement(groups...), true
}

// validateAuthDirectives rejects a malformed @requiresScopes usage at schema
// build, joined into NewSchema's errors like every other Go-vs-SDL shape
// mismatch. gqlparser validates the directive's name, location and required
// argument presence, but never checks the "scopes" value's shape against
// [[String!]!]!: a value one nesting level short of Apollo's syntax (a flat
// list of strings) decodes to a Requirement satisfied by everyone — an AND
// over zero scopes is vacuous — while still creating a site, so the field
// looks guarded and an Authorizer is consulted, but every caller passes.
// Catching this here, once, is what keeps that failure mode from reaching a
// request.
func (b *schemaBuilder) validateAuthDirectives() {
	for name, def := range b.ast.Types {
		if def.BuiltIn {
			continue
		}
		if def.Kind == ast.Object {
			b.checkRequiresScopes(name, def.Directives)
		}
		for _, f := range def.Fields {
			b.checkRequiresScopes(coordinate(name, f.Name), f.Directives)
		}
	}
}

func (b *schemaBuilder) checkRequiresScopes(coord string, ds ast.DirectiveList) {
	d := ds.ForName(authDirective)
	if d == nil {
		return
	}
	if !scopesShapeValid(d.Arguments.ForName("scopes")) {
		b.errorf("%s: @%s scopes must be a non-empty list of non-empty lists of strings", coord, authDirective)
	}
}

// scopesShapeValid reports whether arg's value is a non-empty ListValue of
// non-empty ListValues of StringValue leaves — the literal shape
// [[String!]!]! takes on the wire, since gqlparser does not check it itself.
func scopesShapeValid(arg *ast.Argument) bool {
	if arg == nil || arg.Value == nil || arg.Value.Kind != ast.ListValue || len(arg.Value.Children) == 0 {
		return false
	}
	for _, outer := range arg.Value.Children {
		inner := outer.Value
		if inner == nil || inner.Kind != ast.ListValue || len(inner.Children) == 0 {
			return false
		}
		for _, elem := range inner.Children {
			if elem.Value == nil || elem.Value.Kind != ast.StringValue {
				return false
			}
		}
	}
	return true
}
