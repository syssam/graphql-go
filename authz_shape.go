package graphql

import (
	"slices"
	"strings"

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
	// A plan reaches the same *selectionSet from more than one parent whenever
	// compileSelection's memo collapses a repeated selection, so this is what
	// keeps the walk linear in the plan's DAG rather than in the tree it
	// unfolds to. It is also what stops a shared field being indexed twice,
	// which would be silent: the second index would win. Sharing does not
	// conflate decisions, because a site's requirement is read off the field
	// definition alone and so is the same on every path that reaches it.
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
		// capped is ignored here, not unchecked: resolveAuthRequirements reads
		// this exact ast.FieldDefinition.Directives at NewSchema and already
		// fails the build for any combination requirementOf would report as
		// capped, so a schema that ever reaches plan compile cannot have one.
		if req, ok, _ := requirementOf(f.def.def.Directives); ok {
			f.authIdx = int32(len(b.sites))
			b.sites = append(b.sites, AuthSite{
				Coord:    coordinate(f.def.object.name, f.name),
				Field:    f.def.def,
				Object:   f.def.object.def,
				Kind:     SiteOutput,
				Requires: req,
				leaf:     f.def.leaf,
			})
			b.scopes = append(b.scopes, req.Scopes()...)
		}
	}
	b.walk(f.sub)
}

// requirementOf reads every @requiresScopes occurrence off a definition and
// ANDs them together, through andCapped rather than plain And. A
// `repeatable` directive, or an `extend type`/`extend interface`/`extend
// schema` re-declaring it, adds a second occurrence to the same Directives
// list rather than replacing the first -- gqlparser merges extension
// directives into the base list and skips the non-repeatable check for them
// (confirmed against v2.5.37), and does so even when the directive is not
// declared repeatable -- so reading only ds.ForName's first match silently
// dropped every declaration but the first (round 1), and combining every
// occurrence with plain And silently paid for an unbounded cross product
// before any caller-level cap check ever ran (round 2): four occurrences of
// 30 groups already multiply to 810,000 before resolveAuthRequirements' own
// post-hoc check gets a chance to reject them. Capping every combination step
// here, inside the one function every combination passes through, is what
// makes every caller safe regardless of how many occurrences a definition
// has -- an object's own directives, a field's own, and an interface's or
// interface field's read through combineWithInterfaces's lookup.
//
// NewSchema's validateAuthDirectives rejects any occurrence whose "scopes"
// value is not a non-empty list of non-empty lists of strings, so by the
// time a plan compiles every matched directive's groups are already
// well-formed; this function only extracts and combines them. The
// nil/absent-argument branches below stay as defensive fallbacks, not as the
// shape guarantee.
//
// ok reports whether the directive occurred at all, regardless of how many
// times. capped reports whether combining its occurrences would exceed
// maxRequirementGroups; when capped is true, req is not the true combined
// value and every caller must record a build error rather than treat it as
// this coordinate's real (or a weakened, or a zero) requirement.
func requirementOf(ds ast.DirectiveList) (req Requirement, ok bool, capped bool) {
	occurrences := ds.ForNames(authDirective)
	if len(occurrences) == 0 {
		return Requirement{}, false, false
	}
	for _, d := range occurrences {
		arg := d.Arguments.ForName("scopes")
		if arg == nil || arg.Value == nil {
			continue
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
			continue
		}
		combined, within := andCapped(req, NewRequirement(groups...))
		if !within {
			return req, true, true
		}
		req = combined
	}
	return req, true, false
}

// validateAuthDirectives rejects a malformed or misplaced @requiresScopes at
// schema build, joined into NewSchema's errors like every other Go-vs-SDL
// shape mismatch. gqlparser checks the directive's name, location and
// argument presence but never the "scopes" value's shape: a flat list of
// strings decodes to a requirement satisfied by everyone while still creating
// a site. And a placement the engine does not enforce -- a union, enum, enum
// value, scalar, input object, input field, argument, the schema definition
// itself, or a directive definition's own argument -- would read as guarded
// while guarding nothing, so it is an error rather than a no-op. Executable
// locations (a client document's fields, fragments, and so on) need nothing:
// this directive is declared for type-system locations only.
func (b *schemaBuilder) validateAuthDirectives() {
	b.rejectUnenforced("schema", "the schema definition", b.ast.SchemaDirectives)
	for dname, ddef := range b.ast.Directives {
		for _, a := range ddef.Arguments {
			b.rejectUnenforced(argCoordinate("@"+dname, a.Name), "an argument", a.Directives)
		}
	}
	for name, def := range b.ast.Types {
		if def.BuiltIn {
			continue
		}
		switch def.Kind {
		case ast.Object, ast.Interface:
			b.checkRequiresScopes(name, def.Directives)
			for _, f := range def.Fields {
				coord := coordinate(name, f.Name)
				b.checkRequiresScopes(coord, f.Directives)
				for _, a := range f.Arguments {
					b.rejectUnenforced(argCoordinate(coord, a.Name), "an argument", a.Directives)
				}
			}
		case ast.InputObject:
			b.rejectUnenforced(name, "an input object", def.Directives)
			for _, f := range def.Fields {
				b.rejectUnenforced(coordinate(name, f.Name), "an input field", f.Directives)
			}
		case ast.Enum:
			b.rejectUnenforced(name, "an enum", def.Directives)
			for _, v := range def.EnumValues {
				b.rejectUnenforced(coordinate(name, v.Name), "an enum value", v.Directives)
			}
		case ast.Union:
			b.rejectUnenforced(name, "a union", def.Directives)
		case ast.Scalar:
			b.rejectUnenforced(name, "a scalar", def.Directives)
		}
	}
}

func (b *schemaBuilder) rejectUnenforced(coord, what string, ds ast.DirectiveList) {
	if ds.ForName(authDirective) != nil {
		b.errorf("%s: @%s is not enforced on %s; declare it on an object, interface or field", coord, authDirective, what)
	}
}

// argCoordinate renders a coordinate for an argument, matching the style
// GraphQL error messages already use for one elsewhere in this package (see
// validateDeprecation): Q3.f(a:) for a field argument, @foo(a:) for a
// directive definition's.
func argCoordinate(owner, arg string) string {
	return owner + "(" + arg + ":)"
}

// andCapped ANDs next into acc, refusing to allocate the cross product when
// it would exceed maxRequirementGroups. Requirement.And allocates
// len(acc)*len(next) groups unconditionally: checking the group count only
// after combining (as an earlier version of this function did) still pays
// for that allocation on the way to reporting the error, and four
// interfaces of 30 groups multiply to 810,000 groups before any check runs
// at all, repeated again per field. ok is false when the product would
// exceed the cap -- computed in int64 so a pathological SDL cannot overflow
// the check meant to catch it -- and acc is returned unchanged so the caller
// can stop combining for that coordinate rather than recomputing an
// already-oversized value.
func andCapped(acc, next Requirement) (Requirement, bool) {
	rc, oc := acc.groupCount(), next.groupCount()
	predicted := int64(oc)
	if rc != 0 {
		if oc == 0 {
			predicted = int64(rc)
		} else {
			predicted = int64(rc) * int64(oc)
		}
	}
	if predicted > int64(maxRequirementGroups) {
		return acc, false
	}
	return acc.And(next), true
}

// combineWithInterfaces ANDs acc with the requirement lookup returns for
// each of obj's implemented interfaces, used for both the type-level
// requirement (lookup reads the interface's own directives) and a field's
// (lookup reads the same-named interface field's). Interface names are
// deduplicated first: `type Dog implements Pet` plus `extend type Dog
// implements Pet` gives Interfaces [Pet, Pet], and ANDing Pet's requirement
// with itself would square its own group count rather than counting it
// once. Combining stops at the first product that would exceed the cap,
// reporting one error for coord rather than repeating the same explosion
// for every remaining interface; capped reports whether that happened.
func (b *schemaBuilder) combineWithInterfaces(coord string, acc Requirement, interfaces []string, lookup func(*ast.Definition) ast.DirectiveList) (req Requirement, capped bool) {
	seen := make(map[string]bool, len(interfaces))
	for _, iname := range interfaces {
		if seen[iname] {
			continue
		}
		seen[iname] = true
		idef := b.ast.Types[iname]
		if idef == nil {
			continue
		}
		r, _, rcapped := requirementOf(lookup(idef))
		if rcapped {
			// r is not trustworthy (see requirementOf): the interface's own
			// occurrences already exceeded the cap before we ever got to
			// combine it with acc, so there is nothing valid left to AND.
			b.errorf("%s: effective @%s has more than %d groups", coord, authDirective, maxRequirementGroups)
			return acc, true
		}
		combined, within := andCapped(acc, r)
		if !within {
			b.errorf("%s: effective @%s has more than %d groups", coord, authDirective, maxRequirementGroups)
			return acc, true
		}
		acc = combined
	}
	return acc, false
}

// resolveAuthRequirements stores each bound field's effective requirement on
// its fieldDef and each object's type-level one on its objectType. Every input
// is schema-level and immutable after build, so the value is identical on
// every path that reaches a field -- which is what keeps a memoized selection
// set shared between parents safe to index once.
//
// When an object's own type-level combination already exceeds the cap, its
// fields are skipped entirely rather than each recomputing (and re-reporting)
// the same explosion: every field would inherit the oversized type-level
// value, and one error per field would bury the coordinate that actually
// caused it.
func (b *schemaBuilder) resolveAuthRequirements(s *Schema) {
	for name, obj := range s.objects {
		typeReq, _, typeCapped := requirementOf(obj.def.Directives)
		if typeCapped {
			// typeReq is not trustworthy (see requirementOf): the object's
			// own occurrences alone already exceeded the cap, so there is
			// nothing valid to combine with its interfaces or hand to any
			// field, and obj.requires is left at its zero value.
			b.errorf("%s: effective @%s has more than %d groups", name, authDirective, maxRequirementGroups)
			continue
		}
		typeReq, capped := b.combineWithInterfaces(name, typeReq, obj.def.Interfaces, func(idef *ast.Definition) ast.DirectiveList {
			return idef.Directives
		})
		obj.requires = typeReq
		if capped {
			continue
		}
		if typeReq.groupCount() > maxRequirementGroups {
			b.errorf("%s: effective @%s has %d groups, more than %d", name, authDirective, typeReq.groupCount(), maxRequirementGroups)
			continue
		}

		for _, fd := range obj.fields {
			coord := coordinate(name, fd.name)
			req, _, fieldCapped := requirementOf(fd.def.Directives)
			if fieldCapped {
				b.errorf("%s: effective @%s has more than %d groups", coord, authDirective, maxRequirementGroups)
				continue
			}
			req, within := andCapped(req, typeReq)
			if !within {
				b.errorf("%s: effective @%s has more than %d groups", coord, authDirective, maxRequirementGroups)
				continue
			}
			req, _ = b.combineWithInterfaces(coord, req, obj.def.Interfaces, func(idef *ast.Definition) ast.DirectiveList {
				if ifd := idef.Fields.ForName(fd.name); ifd != nil {
					return ifd.Directives
				}
				return nil
			})
			fd.requires = req
		}
	}
}

// validateAuthCoverage fails the build for any bound field that declares
// neither @requiresScopes nor @public, when RequireAuthCoverage is on. It
// walks s.objects (concrete, bound object types) rather than the raw SDL:
// validateCoverage already rejects an unbound type or field regardless of
// this option, so every field reachable here is guaranteed to have a
// fieldDef, and neither an interface's nor an object's own @requiresScopes is
// treated as covering a field -- requirementOf is read only off a field
// definition in shapeBuilder.field, and neither SiteObject nor an
// ObjectAuthorizer is ever constructed, so an object-level declaration builds
// no enforcement site (interface fields are the same limitation, Task 4).
// Accepting either here would certify a field authorization never actually
// checks, which is the false coverage this primitive exists to prevent.
// Object-level @public is unaffected: it has always meant "every field here
// is exempt", not "every field here is guarded", so it still exempts.
func (b *schemaBuilder) validateAuthCoverage(s *Schema) {
	if !b.authCoverage {
		return
	}
	for name, obj := range s.objects {
		if strings.HasPrefix(name, "__") {
			continue
		}
		if obj.def.Directives.ForName("public") != nil {
			continue
		}
		// Coverage only asks whether the directive occurred (ok); resolveAuthRequirements
		// has already run and independently rejected the build if either of these
		// occurrences was capped, so capped is discarded here rather than re-checked.
		_, objDeclared, _ := requirementOf(obj.def.Directives)
		for _, fd := range obj.fields {
			if strings.HasPrefix(fd.name, "__") {
				continue
			}
			if fd.def.Directives.ForName("public") != nil {
				continue
			}
			if _, ok, _ := requirementOf(fd.def.Directives); ok {
				continue
			}
			if objDeclared {
				b.errorf("field %s declares no authorization; the object-level @requiresScopes on %s is not yet enforced, add a field-level @requiresScopes or @public", coordinate(obj.name, fd.name), obj.name)
				continue
			}
			b.errorf("field %s declares no authorization; add @requiresScopes or @public", coordinate(obj.name, fd.name))
		}
	}
}

// checkRequiresScopes shape-checks every @requiresScopes occurrence on coord,
// not just the first ds.ForName match: a repeatable directive or an
// extension can add a second occurrence gqlparser never folds into the
// first (see requirementOf), and a malformed one must fail the build exactly
// like a malformed first occurrence would.
func (b *schemaBuilder) checkRequiresScopes(coord string, ds ast.DirectiveList) {
	for _, d := range ds.ForNames(authDirective) {
		if !scopesShapeValid(d.Arguments.ForName("scopes")) {
			b.errorf("%s: @%s scopes must be a non-empty list of non-empty lists of strings", coord, authDirective)
		}
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
