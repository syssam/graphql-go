package graphql

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// AbstractOption tunes an Interface or Union binding.
type AbstractOption interface {
	applyAbstract(*abstractBinding)
}

type abstractOptFunc func(*abstractBinding)

func (f abstractOptFunc) applyAbstract(ab *abstractBinding) { f(ab) }

// TypeResolver overrides dynamic Go type lookup with an explicit function
// returning the concrete GraphQL object type name for a value.
func TypeResolver[T any](fn func(T) string) AbstractOption {
	return abstractOptFunc(func(ab *abstractBinding) {
		ab.resolverType = reflect.TypeFor[T]()
		ab.resolveType = func(v any) string { return fn(v.(T)) }
	})
}

type abstractBinding struct {
	name        string
	goType      reflect.Type
	resolveType func(any) string
	// resolverType is the TypeResolver's parameter type. Every value at the
	// position is asserted to it, so it has to take whatever the binding's
	// own type can hold.
	resolverType reflect.Type
}

// resolverTakes reports whether a TypeResolver over rt can be handed every
// value of the binding's type: the same type, or an interface it implements.
func resolverTakes(rt, bound reflect.Type) bool {
	return rt == bound || (rt.Kind() == reflect.Interface && bound.Implements(rt))
}

// Interface binds the GraphQL interface name to the Go type T, typically a
// Go interface. Binding is optional: unbound abstract types resolve their
// concrete type from the dynamic Go type of the value.
func Interface[T any](name string, opts ...AbstractOption) SchemaOption {
	return bindAbstract[T](name, ast.Interface, opts)
}

// Union binds the GraphQL union name to the Go type T.
func Union[T any](name string, opts ...AbstractOption) SchemaOption {
	return bindAbstract[T](name, ast.Union, opts)
}

func bindAbstract[T any](name string, kind ast.DefinitionKind, opts []AbstractOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Types[name]
		if def == nil || def.Kind != kind {
			b.errorf("%s %q: type is not %s in the schema", kindLabel(kind), name, articleFor(kind))
			return
		}
		if _, dup := b.abstracts[name]; dup {
			b.errorf("%s %q: bound more than once", kindLabel(kind), name)
			return
		}
		ab := &abstractBinding{name: name, goType: reflect.TypeFor[T]()}
		for _, o := range opts {
			o.applyAbstract(ab)
		}
		if ab.resolverType != nil && !resolverTakes(ab.resolverType, ab.goType) {
			b.errorf("%s %q: its TypeResolver takes %s, but the binding is for %s, and a value of any other type at this position would not reach it",
				kindLabel(kind), name, ab.resolverType, ab.goType)
			return
		}
		b.abstracts[name] = ab
		registerAbstractShapes[T](b.reg)
	})
}

func kindLabel(kind ast.DefinitionKind) string {
	if kind == ast.Union {
		return "Union"
	}
	return "Interface"
}

func articleFor(kind ast.DefinitionKind) string {
	if kind == ast.Union {
		return "a union"
	}
	return "an interface"
}

// abstractType is a resolved interface or union.
type abstractType struct {
	name        string
	def         *ast.Definition
	goType      reflect.Type
	resolveType func(any) string
	possible    map[string]*objectType
}

func (b *schemaBuilder) resolveAbstract(s *Schema, name string, def *ast.Definition) *abstractType {
	at := &abstractType{name: name, def: def, possible: make(map[string]*objectType)}
	for _, pd := range b.ast.GetPossibleTypes(def) {
		if obj := s.objects[pd.Name]; obj != nil {
			at.possible[pd.Name] = obj
		}
	}
	if ab := b.abstracts[name]; ab != nil {
		at.goType = ab.goType
		at.resolveType = ab.resolveType
	}
	return at
}

// concreteType determines the object type of a value returned for an
// abstract position, using the TypeResolver when one is bound and the
// dynamic Go type otherwise.
func (s *Schema) concreteType(at *abstractType, v any) (*objectType, error) {
	if at.resolveType != nil {
		name := at.resolveType(v)
		obj := at.possible[name]
		if obj == nil {
			return nil, fmt.Errorf("abstract type %s resolved to %q, which is not one of its possible types", at.name, name)
		}
		return obj, nil
	}
	return s.objectForGoType(at, reflect.TypeOf(v), v)
}

// objectForGoType picks the unique possible object bound to t. Several
// GraphQL objects may share a Go type; that is only an error when more
// than one of them is a possible type of at.
func (s *Schema) objectForGoType(at *abstractType, t reflect.Type, v any) (*objectType, error) {
	var matches []*objectType
	for _, obj := range s.goTypes[t] {
		if _, ok := at.possible[obj.name]; ok {
			matches = append(matches, obj)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(s.goTypes[t]) == 0 {
			return nil, fmt.Errorf("abstract type %s must resolve to an object type at runtime; Go type %T is not bound", at.name, v)
		}
		return nil, fmt.Errorf("abstract type %s must resolve to one of its possible types; Go type %T is bound to %s", at.name, v, s.goTypes[t][0].name)
	default:
		names := make([]string, len(matches))
		for i, obj := range matches {
			names[i] = obj.name
		}
		return nil, fmt.Errorf("abstract type %s is ambiguous for Go type %T (bound to %s); set a TypeResolver", at.name, v, strings.Join(names, ", "))
	}
}
