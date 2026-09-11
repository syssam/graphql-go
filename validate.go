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
func checkOutputLeafShape(r *registry, key typeKey, sdl *ast.Type) error {
	info, ok := r.shapes[key]
	if !ok {
		if _, known := r.leafKinds[key.name]; !known {
			return fmt.Errorf("%s %s has no Scalar or Enum binding", leafKindName(r, key.name), key.name)
		}
		return fmt.Errorf("Go type %s is not registered for %s; expected one of E, *E, []E or []*E where E is a bound Go type", key.typ, key.name)
	}
	if want := listDepth(sdl); info.depth != want {
		return fmt.Errorf("Go type %s has %d list level(s) but %s has %d", key.typ, info.depth, sdl.String(), want)
	}
	return nil
}

// checkInputShape verifies depth and nullability compatibility of an input
// shape: every nullable SDL level must be representable as null in Go.
func checkInputShape(r *registry, key typeKey, sdl *ast.Type) error {
	info, ok := r.shapes[key]
	if !ok {
		if _, isInput := r.inputsByName[key.name]; !isInput {
			if _, known := r.leafKinds[key.name]; !known {
				return fmt.Errorf("type %s has no Scalar, Enum or Input binding", key.name)
			}
		}
		return fmt.Errorf("Go type %s is not registered for %s; expected one of E, *E, []E or []*E where E is a bound Go type", key.typ, key.name)
	}
	if want := listDepth(sdl); info.depth != want {
		return fmt.Errorf("Go type %s has %d list level(s) but %s has %d", key.typ, info.depth, sdl.String(), want)
	}
	t := sdl
	for level := 0; t != nil; level++ {
		if !t.NonNull && !info.nullable[level] {
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
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return nil, fmt.Errorf("Go type %s has fewer list levels than %s", goType, sdl.String())
		}
		t = t.Elem()
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
		obj := s.goTypes[t]
		if obj == nil {
			return nil, fmt.Errorf("Go type %s returned for abstract type %s is not bound to any object type", t, def.Name)
		}
		if at := s.abstracts[def.Name]; at != nil {
			if _, ok := at.possible[obj.name]; !ok {
				return nil, fmt.Errorf("Go type %s is bound to %s, which is not a possible type of %s", t, obj.name, def.Name)
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("type %s is %s and cannot be a composite field type", def.Name, def.Kind)
}

func leafKindName(r *registry, name string) string {
	if r.leafKinds[name] == ast.Enum {
		return "enum"
	}
	return "scalar"
}
