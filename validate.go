package graphql

import (
	"fmt"
	"reflect"

	"github.com/vektah/gqlparser/v2/ast"
)

// Validate builds the schema and discards it. It exists for tests that want
// to assert bindings are complete and shape-compatible without executing
// anything.
func Validate(src Source, opts ...SchemaOption) error {
	_, err := NewSchema(src, opts...)
	return err
}

// checkOutputLeafShape verifies that a registered leaf shape exists for key
// and that its list depth matches the SDL type.
func checkOutputLeafShape(r *registry, key typeKey, sdl *ast.Type, kind ast.DefinitionKind) error {
	info, ok := r.shapes[key]
	if !ok {
		if _, known := r.leafKinds[key.name]; !known {
			return missingLeafBinding(kind, key)
		}
		return fmt.Errorf("Go type %s is not registered for %s; expected E, *E or up to two list levels of them where E is a bound Go type", key.typ, key.name)
	}
	if want := listDepth(sdl); info.depth != want {
		return fmt.Errorf("Go type %s has %d list level(s) but %s has %d", key.typ, info.depth, sdl.String(), want)
	}
	return nil
}

// checkInputShape verifies depth and nullability compatibility of an input
// shape: every nullable SDL level must be representable as null in Go,
// unless the binding set ZeroForNull and accepts the zero value there.
func checkInputShape(r *registry, key typeKey, sdl *ast.Type, zeroForNull bool) error {
	info, ok := r.shapes[key]
	if !ok {
		if _, isInput := r.inputsByName[key.name]; !isInput {
			if _, known := r.leafKinds[key.name]; !known {
				return fmt.Errorf("type %s has no Scalar, Enum or Input binding", key.name)
			}
		}
		return fmt.Errorf("Go type %s is not registered for %s; expected E, *E or up to two list levels of them where E is a bound Go type", key.typ, key.name)
	}
	if want := listDepth(sdl); info.depth != want {
		return fmt.Errorf("Go type %s has %d list level(s) but %s has %d", key.typ, info.depth, sdl.String(), want)
	}
	t := sdl
	for level := 0; t != nil; level++ {
		if !t.NonNull && !info.nullable[level] && !zeroForNull {
			return fmt.Errorf("Go type %s cannot represent null at level %d of nullable type %s; use a pointer or slice there", key.typ, level, sdl.String())
		}
		t = t.Elem
	}
	return nil
}

// checkOutputCompositeShape verifies that the Go result type matches the
// SDL list depth and, for object types, resolves to the bound Go type. It
// returns the target object type, or nil for abstract types.
func checkOutputCompositeShape(s *Schema, goType reflect.Type, sdl *ast.Type) (*objectType, error) {
	t := goType
	for sdlT := sdl; sdlT.Elem != nil; sdlT = sdlT.Elem {
		if e, isSeq := seqElem(t); isSeq {
			// Without a registered traverser the executor would fall back to the
			// reflective one, which cannot index a func and panics after the
			// resolver has already returned.
			if _, ok := s.reg.traversers[t]; !ok {
				return nil, fmt.Errorf("Go type %s cannot be traversed: iter.Seq is supported only as the innermost list level over a bound Go type", t)
			}
			t = e
			continue
		}
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return nil, fmt.Errorf("Go type %s has fewer list levels than %s", goType, sdl.String())
		}
		t = t.Elem()
	}
	if _, isSeq := seqElem(t); isSeq {
		return nil, fmt.Errorf("Go type %s has more list levels than %s", goType, sdl.String())
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		return nil, fmt.Errorf("Go type %s has more list levels than %s", goType, sdl.String())
	}

	def := s.ast.Types[sdl.Name()]
	if def == nil {
		return nil, fmt.Errorf("type %s is not defined in the schema", sdl.Name())
	}
	switch def.Kind {
	case ast.Object:
		obj := s.objects[def.Name]
		if obj == nil {
			return nil, fmt.Errorf("type %s has no Object binding", def.Name)
		}
		if t != obj.shapes.elem && t != obj.shapes.ptr {
			return nil, fmt.Errorf("Go type %s does not match Object[%s] bound to %s (expected %s or %s)", goType, def.Name, obj.shapes.elem, obj.shapes.ptr, obj.shapes.elem)
		}
		return obj, nil
	case ast.Interface, ast.Union:
		if t.Kind() == reflect.Interface {
			return nil, nil
		}
		objs := s.goTypes[t]
		if len(objs) == 0 {
			return nil, fmt.Errorf("Go type %s returned for abstract type %s is not bound to any object type", t, def.Name)
		}
		if at := s.abstracts[def.Name]; at != nil {
			n := 0
			for _, obj := range objs {
				if _, ok := at.possible[obj.name]; ok {
					n++
				}
			}
			if n == 0 {
				return nil, fmt.Errorf("Go type %s is bound to %s, which is not a possible type of %s", t, objs[0].name, def.Name)
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("type %s is %s and cannot be a composite field type", def.Name, def.Kind)
}

// missingLeafBinding names the option that would bind key, with the Go type
// the field already uses, so the fix can be pasted rather than worked out.
// gqlc emits a Go type for every unmapped custom scalar but cannot know its
// wire format, so this is the error a generated schema meets first.
func missingLeafBinding(kind ast.DefinitionKind, key typeKey) error {
	elem := key.typ
	for elem != nil && (elem.Kind() == reflect.Pointer || elem.Kind() == reflect.Slice) {
		elem = elem.Elem()
	}
	if kind == ast.Enum {
		return fmt.Errorf("enum %s has no binding; add graphql.Enum[%s](%q, map[%s]string{...}) to the NewSchema options",
			key.name, elem, key.name, elem)
	}
	return fmt.Errorf("scalar %s has no binding; add graphql.Scalar[%s](%q, marshal, unmarshal) to the NewSchema options",
		key.name, elem, key.name)
}
