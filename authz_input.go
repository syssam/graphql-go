package graphql

import (
	"maps"
	"slices"

	"github.com/vektah/gqlparser/v2/ast"
)

// InputKey is one position a client supplied inside an argument that an
// Authorizer is asked about: the input-object keys from the argument down,
// list indices omitted. Enum holds the value when that position is an enum,
// because ordering and grouping name a column by enum value rather than by
// key. Null is set for an explicit null, which is a write where an absent key
// is not. Scalar values are never carried: a policy decides on schema
// identifiers, and the values are user data.
//
// Path shares its storage with the paths of the other keys reported for the
// same request: a key's path is usually the next key's prefix, and an
// argument nested n deep would otherwise hold n paths of n elements. Read it,
// and append to it freely -- an append never reaches another key -- but do not
// assign to its elements.
type InputKey struct {
	Path []string
	Enum string
	Null bool
}

// maxInputPathElems bounds the path storage one request's argument sites may
// allocate between them, in path elements.
//
// Sharing prefixes makes a chain of n keys cost between 2n and 4n elements,
// but a key whose path no deeper key continues owns a path of its own, and an
// input type that refers to itself lets a client send one of those at every
// depth: {a: 1, not: {a: 1, not: ...}} reports n paths of up to n elements
// whatever the walk does, because InputKey.Path is a slice per key. How deep
// that goes is the client's choice and not the schema's, so past this the
// operation is refused rather than paid for.
//
// The number is eight times what the deepest chain the variables decoder
// admits needs (10 000 levels, about 33 000 elements), so no depth is
// refused as such, and about 4 MiB of string headers. List indices are not
// part of a path and a repeated position is reported once, so an input type
// that does not refer to itself cannot approach it: its distinct paths are
// the schema's own.
const maxInputPathElems = 1 << 18

// inputKeys walks the value a client supplied for one argument. It reads the
// operation's AST and variables rather than an argument map, because an
// argument map has SDL defaults filled in and a default is not something the
// client chose. A default on an operation variable is, so a variable resolved
// from its default is walked like any other.
//
// budget is what is left of maxInputPathElems for this request. ok is false
// when the walk stopped because it ran out, and the keys are then incomplete:
// the caller must refuse the operation, since a policy shown part of an
// argument would be deciding on less than the resolver receives.
func inputKeys(types map[string]*ast.Definition, t *ast.Type, v *ast.Value, vars map[string]any, budget *int) (keys []InputKey, ok bool) {
	w := inputWalk{types: types, vars: vars, budget: budget}
	w.ast(t, v, &inputPos{})
	return w.keys, !w.over()
}

// argumentInputs walks what the request supplied at each of the shape's
// argument sites, indexed by site. The error is for the client: its input
// names more distinct key paths than maxInputPathElems allows, and the
// operation is refused rather than authorized on part of it.
func (s *AuthShape) argumentInputs(types map[string]*ast.Definition, vars map[string]any) ([][]InputKey, error) {
	inputs := make([][]InputKey, len(s.sites))
	budget := maxInputPathElems
	// By index: an AuthSite is 136 bytes and this runs per request, so
	// ranging by value memmoves the whole table every time.
	for i := range s.sites {
		site := &s.sites[i]
		if site.argValue == nil {
			continue
		}
		keys, ok := inputKeys(types, site.argType, site.argValue, vars, &budget)
		if !ok {
			return nil, Errorf("The value supplied for %s nests too many distinct keys to be authorized.", site.Coord).WithCode(CodeBadUserInput)
		}
		inputs[i] = keys
	}
	return inputs, nil
}

type inputWalk struct {
	types  map[string]*ast.Definition
	vars   map[string]any
	keys   []InputKey
	budget *int
}

// over reports that the request's budget is spent, which ends the walk: it
// only goes down, so once below zero it stays there.
func (w *inputWalk) over() bool { return *w.budget < 0 }

// inputPos is one position in the argument: the keys from the argument down
// to it. Positions form a tree, so two values at the same path -- two elements
// of one list, most often -- meet at one node, and that node is where it is
// recorded that the position has been reported. The walk used to join each
// path into a string to ask that, and to clone the path once to descend and
// again to report, which made an argument nested n deep cost n*n.
type inputPos struct {
	// path is nil at the argument itself, as a reported key's Path is there.
	path []string
	kids map[string]*inputPos

	// extended records that a child has taken the element after path in the
	// array path lives in. The first child of a position extends its path in
	// place; any other needs an array of its own, or it would overwrite the
	// first one's last element.
	extended bool

	// plain, null and enums are what has been reported here already: the key
	// alone, an explicit null, and each enum value.
	plain, null bool
	enums       map[string]struct{}
}

// child returns the position one key below p, or nil when its path would
// take more than the request has left; the walk is then over.
func (w *inputWalk) child(p *inputPos, name string) *inputPos {
	if c := p.kids[name]; c != nil {
		return c
	}
	c := &inputPos{}
	n := len(p.path)
	if !p.extended && n < cap(p.path) {
		p.extended = true
		c.path = p.path[:n+1]
		c.path[n] = name
	} else {
		// Doubling is what keeps a chain linear: it is copied at lengths that
		// double, so the copies sum to twice its length.
		grown := 2 * (n + 1)
		if *w.budget -= grown; w.over() {
			return nil
		}
		c.path = make([]string, n+1, grown)
		copy(c.path, p.path)
		c.path[n] = name
	}
	if p.kids == nil {
		p.kids = make(map[string]*inputPos, 1)
	}
	p.kids[name] = c
	return c
}

// report appends a key for p. The path is capped at its own length so that an
// Authorizer appending to it gets a new array instead of writing over the
// element a deeper key's path holds there.
func (w *inputWalk) report(p *inputPos, enum string, null bool) {
	w.keys = append(w.keys, InputKey{Path: p.path[:len(p.path):len(p.path)], Enum: enum, Null: null})
}

func (w *inputWalk) addKey(p *inputPos) {
	if !p.plain {
		p.plain = true
		w.report(p, "", false)
	}
}

func (w *inputWalk) addNull(p *inputPos) {
	if !p.null {
		p.null = true
		w.report(p, "", true)
	}
}

func (w *inputWalk) addEnum(p *inputPos, value string) {
	if _, seen := p.enums[value]; seen {
		return
	}
	if p.enums == nil {
		p.enums = make(map[string]struct{}, 1)
	}
	p.enums[value] = struct{}{}
	w.report(p, value, false)
}

// atArgument reports whether p is the argument itself rather than a key
// inside it. A scalar there is the argument's whole value, which is user data
// with no key to name it.
func (p *inputPos) atArgument() bool { return len(p.path) == 0 }

// isComposite reports whether v's supplied value is an object or a list,
// literal or through a variable: an object/list value at a key makes that
// key itself a reportable position, on top of whatever it contains.
func (w *inputWalk) isComposite(v *ast.Value) bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case ast.ObjectValue, ast.ListValue:
		return true
	case ast.Variable:
		raw, ok := w.vars[v.Raw]
		if !ok {
			return false
		}
		switch raw.(type) {
		case map[string]any, []any:
			return true
		}
	}
	return false
}

func (w *inputWalk) ast(t *ast.Type, v *ast.Value, p *inputPos) {
	if v == nil || w.over() {
		return
	}
	switch v.Kind {
	case ast.Variable:
		raw, ok := w.vars[v.Raw]
		if !ok {
			return
		}
		w.raw(t, raw, p)
	case ast.NullValue:
		w.addNull(p)
	case ast.ListValue:
		elem := t
		if t.Elem != nil {
			elem = t.Elem
		}
		for _, c := range v.Children {
			// A null element is not a position of its own: it says nothing
			// about the key the list lives under, which is reported (or
			// not) independently of what the list contains.
			if c.Value != nil && c.Value.Kind == ast.NullValue {
				continue
			}
			w.ast(elem, c.Value, p)
		}
	case ast.ObjectValue:
		def := w.types[t.Name()]
		if def == nil {
			return
		}
		for _, c := range v.Children {
			fd := def.Fields.ForName(c.Name)
			if fd == nil {
				continue
			}
			child := w.child(p, c.Name)
			if child == nil {
				return
			}
			if w.isComposite(c.Value) {
				w.addKey(child)
			}
			w.ast(fd.Type, c.Value, child)
		}
	case ast.EnumValue:
		// A custom scalar accepts a bare identifier, which parses to the
		// same ast.EnumValue kind as a real enum literal; only a value whose
		// named type is actually an enum in the schema is an enum here.
		if def := w.types[t.Name()]; def != nil && def.Kind == ast.Enum {
			w.addEnum(p, v.Raw)
		} else if !p.atArgument() {
			w.addKey(p)
		}
	default:
		if !p.atArgument() {
			w.addKey(p)
		}
	}
}

func (w *inputWalk) raw(t *ast.Type, v any, p *inputPos) {
	if w.over() {
		return
	}
	if v == nil {
		w.addNull(p)
		return
	}
	if t.Elem != nil {
		if list, ok := v.([]any); ok {
			for _, e := range list {
				// See the matching skip in ast(): a null element is not a
				// position, and reporting it as one would make an
				// explicit-null write indistinguishable from a stray nil in
				// a filter list.
				if e == nil {
					continue
				}
				w.raw(t.Elem, e, p)
			}
			return
		}
		// Input coercion accepts a single value where a list is expected.
		w.raw(t.Elem, v, p)
		return
	}
	def := w.types[t.Name()]
	if def == nil {
		return
	}
	switch def.Kind {
	case ast.Enum:
		// A value coercion would reject is not one the client meaningfully
		// supplied at this position; report nothing rather than a
		// fabricated identifier.
		if s, ok := v.(string); ok && def.EnumValues.ForName(s) != nil {
			w.addEnum(p, s)
		}
	case ast.InputObject:
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		// Sorted so the reported order does not depend on Go's randomized
		// map iteration.
		for _, name := range slices.Sorted(maps.Keys(m)) {
			fd := def.Fields.ForName(name)
			if fd == nil {
				continue
			}
			fv := m[name]
			child := w.child(p, name)
			if child == nil {
				return
			}
			switch fv.(type) {
			case map[string]any, []any:
				w.addKey(child)
			}
			w.raw(fd.Type, fv, child)
		}
	default:
		if !p.atArgument() {
			w.addKey(p)
		}
	}
}
