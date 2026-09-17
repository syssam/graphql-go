package graphql

import (
	"maps"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// InputKey is one position a client supplied inside an argument that an
// Authorizer is asked about: the input-object keys from the argument down,
// list indices omitted. Enum holds the value when that position is an enum,
// because ordering and grouping name a column by enum value rather than by
// key. Null is set for an explicit null, which is a write where an absent key
// is not. Scalar values are never carried: a policy decides on schema
// identifiers, and the values are user data.
type InputKey struct {
	Path []string
	Enum string
	Null bool
}

// inputKeys walks the value a client supplied for one argument. It reads the
// operation's AST and variables rather than an argument map, because an
// argument map has SDL defaults filled in and a default is not something the
// client chose. A default on an operation variable is, so a variable resolved
// from its default is walked like any other.
func inputKeys(types map[string]*ast.Definition, t *ast.Type, v *ast.Value, vars map[string]any) []InputKey {
	w := inputWalk{types: types, vars: vars, seen: map[string]bool{}}
	w.ast(t, v, nil)
	return w.keys
}

type inputWalk struct {
	types map[string]*ast.Definition
	vars  map[string]any
	keys  []InputKey
	seen  map[string]bool
}

func (w *inputWalk) add(path []string, enum string, null bool) {
	id := strings.Join(path, "\x00") + "\x01" + enum
	if null {
		id += "\x02"
	}
	if w.seen[id] {
		return
	}
	w.seen[id] = true
	w.keys = append(w.keys, InputKey{Path: slices.Clone(path), Enum: enum, Null: null})
}

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

func (w *inputWalk) ast(t *ast.Type, v *ast.Value, path []string) {
	if v == nil {
		return
	}
	switch v.Kind {
	case ast.Variable:
		raw, ok := w.vars[v.Raw]
		if !ok {
			return
		}
		w.raw(t, raw, path)
	case ast.NullValue:
		w.add(path, "", true)
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
			w.ast(elem, c.Value, path)
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
			child := append(slices.Clone(path), c.Name)
			if w.isComposite(c.Value) {
				w.add(child, "", false)
			}
			w.ast(fd.Type, c.Value, child)
		}
	case ast.EnumValue:
		// A custom scalar accepts a bare identifier, which parses to the
		// same ast.EnumValue kind as a real enum literal; only a value whose
		// named type is actually an enum in the schema is an enum here.
		if def := w.types[t.Name()]; def != nil && def.Kind == ast.Enum {
			w.add(path, v.Raw, false)
		} else if len(path) > 0 {
			w.add(path, "", false)
		}
	default:
		if len(path) > 0 {
			w.add(path, "", false)
		}
	}
}

func (w *inputWalk) raw(t *ast.Type, v any, path []string) {
	if v == nil {
		w.add(path, "", true)
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
				w.raw(t.Elem, e, path)
			}
			return
		}
		// Input coercion accepts a single value where a list is expected.
		w.raw(t.Elem, v, path)
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
			w.add(path, s, false)
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
			child := append(slices.Clone(path), name)
			switch fv.(type) {
			case map[string]any, []any:
				w.add(child, "", false)
			}
			w.raw(fd.Type, fv, child)
		}
	default:
		if len(path) > 0 {
			w.add(path, "", false)
		}
	}
}
