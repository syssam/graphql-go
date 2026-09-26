package graphql

import (
	"context"
	"fmt"
	"reflect"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// FieldOption declares one field of an Object binding.
type FieldOption interface {
	applyField(*objectBinding)
}

// FieldSchedule tunes whether a field runs inline or concurrently. The
// name follows the gRPC CallOption style: it only affects scheduling.
type FieldSchedule func(*fieldSpec)

// Inline forces a Resolve field to run synchronously in the parent's
// goroutine even when siblings are scheduled concurrently.
func Inline() FieldSchedule { return func(f *fieldSpec) { f.inline = true } }

// Concurrent allows a Field, which is normally executed inline, to be
// scheduled concurrently. Use it for pure fields that are CPU-heavy.
func Concurrent() FieldSchedule { return func(f *fieldSpec) { f.concurrent = true } }

// objectBinding collects the field specifications registered for one
// GraphQL object type across all Object calls that name it.
type objectBinding struct {
	name      string
	shapes    objectShapes
	fields    []*fieldSpec
	attempted map[string]bool // fields with a binding, even if it failed to compose
}

// fieldSpec is a field binding before it is composed against the SDL.
type fieldSpec struct {
	name       string
	parent     reflect.Type
	result     reflect.Type
	argsType   reflect.Type
	pure       bool
	inline     bool
	concurrent bool
	calls      func(obj *objectType) (fieldCalls, error)
	// compose, when set, replaces composeField: a subscription field is
	// composed as a Root field and then given its stream opener.
	compose func(b *schemaBuilder, s *Schema, obj *objectType, def *ast.FieldDefinition, fd *fieldDef) error
}

func (f *fieldSpec) applyField(ob *objectBinding) { ob.fields = append(ob.fields, f) }

// fieldDef is a fully composed field executor.
type fieldDef struct {
	name        string
	def         *ast.FieldDefinition
	typ         *ast.Type
	object      *objectType
	leaf        bool
	pure        bool
	schedulable bool
	wrapped     bool
	args        *inputDecoder

	// Leaf fields write their value directly.
	writeLeaf func(ctx context.Context, w *jsonw.Writer, parent, args any, fc *FieldContext) error
	writeAny  func(w *jsonw.Writer, v any, t *ast.Type) error

	// Composite fields return a value the executor traverses.
	resolve func(ctx context.Context, parent, args any, fc *FieldContext) (any, error)
	shape   *valueShape

	// anyResolve is the type-erased form used by directives and interceptors.
	anyResolve FieldFunc

	// subscribe opens the source event stream; set on subscription root
	// fields only, by Subscribe and SubscribeArgs.
	subscribe subscribeFunc

	// requires is the field's effective authorization requirement: its own
	// @requiresScopes AND its object type's AND every implemented
	// interface's type-level and same-named field requirement. It is
	// computed once in NewSchema so plan compile and RequireAuthCoverage
	// read the same value; the zero value means nothing is required.
	requires Requirement
}

// wrap replaces the field's executor with wrapper(previous), switching the
// field to the type-erased path.
func (fd *fieldDef) wrap(wrapper func(FieldFunc) FieldFunc) {
	next := fd.anyResolve
	fd.anyResolve = wrapper(next)
	fd.wrapped = true
	if fd.leaf {
		resolve, writeAny, typ := fd.anyResolve, fd.writeAny, fd.typ
		fd.writeLeaf = func(ctx context.Context, w *jsonw.Writer, parent, args any, _ *FieldContext) error {
			v, err := resolve(ctx, parent, args)
			if err != nil {
				return err
			}
			return writeAny(w, v, typ)
		}
		return
	}
	// fd.resolve must match fieldExec's shape, but anyResolve is FieldFunc --
	// the type directives and interceptors share -- so a composite field's
	// resolve needs this adapter where a leaf's writeLeaf does not.
	resolve := fd.anyResolve
	fd.resolve = func(ctx context.Context, parent, args any, _ *FieldContext) (any, error) {
		return resolve(ctx, parent, args)
	}
}

// Object binds the GraphQL object type name to the Go type T. Field
// functions receive *T (or T, which costs a copy). T must not itself be a
// pointer type. Several Object calls for the same name are merged.
func Object[T any](name string, fields ...FieldOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		bindObject(b, name, reflect.TypeFor[T](), func() objectShapes { return registerObjectShapes[T](b.reg) }, fields)
	})
}

func bindObject(b *schemaBuilder, name string, tE reflect.Type, shapes func() objectShapes, fields []FieldOption) {
	if tE.Kind() == reflect.Pointer {
		b.errorf("Object %q: bind the element type %s, not the pointer type %s", name, tE.Elem(), tE)
		return
	}
	ob := b.objects[name]
	if ob == nil {
		ob = &objectBinding{name: name, shapes: shapes(), attempted: make(map[string]bool)}
		b.objects[name] = ob
		b.objectOrder = append(b.objectOrder, name)
	} else if ob.shapes.elem != tE {
		b.errorf("Object %q: bound to both %s and %s", name, ob.shapes.elem, tE)
		return
	}
	for _, f := range fields {
		f.applyField(ob)
	}
}

// Field binds a pure field: fn reads data from the parent without I/O. Pure
// fields are executed inline and never scheduled on their own goroutine.
// A composite list result may be spelled iter.Seq[E] or iter.Seq[*E] wherever
// it may be spelled []E or []*E.
func Field[P, R any](name string, fn func(P) R, opts ...FieldSchedule) FieldOption {
	return newFieldSpec[P, R](name, nil, true, opts, func(_ context.Context, p P, _ any) (R, error) {
		return fn(p), nil
	})
}

// FieldArgs binds a pure field that takes arguments decoded into A.
func FieldArgs[P, A, R any](name string, fn func(P, A) R, opts ...FieldSchedule) FieldOption {
	return newFieldSpec[P, R](name, reflect.TypeFor[A](), true, opts, func(_ context.Context, p P, args any) (R, error) {
		return fn(p, *args.(*A)), nil
	})
}

// Resolve binds a resolver field that may perform I/O. Resolver fields are
// eligible for concurrent scheduling. A composite list result may be spelled
// iter.Seq[E] or iter.Seq[*E] wherever it may be spelled []E or []*E.
func Resolve[P, R any](name string, fn func(context.Context, P) (R, error), opts ...FieldSchedule) FieldOption {
	return newFieldSpec[P, R](name, nil, false, opts, func(ctx context.Context, p P, _ any) (R, error) {
		return fn(ctx, p)
	})
}

// ResolveArgs binds a resolver field that takes arguments decoded into A.
// An Args[A] registration must be present in the same schema.
func ResolveArgs[P, A, R any](name string, fn func(context.Context, P, A) (R, error), opts ...FieldSchedule) FieldOption {
	return newFieldSpec[P, R](name, reflect.TypeFor[A](), false, opts, func(ctx context.Context, p P, args any) (R, error) {
		return fn(ctx, p, *args.(*A))
	})
}

// newFieldSpec is the generic half of a field binding: the closures that call
// the bound function with its parent converted to P and write or box its R.
// Everything else about composing a field is composeField, which is not
// generic -- a package binding hundreds of fields, as generated code does,
// would otherwise compile that logic again for every one of them.
func newFieldSpec[P, R any](name string, argsType reflect.Type, pure bool, opts []FieldSchedule, call func(context.Context, P, any) (R, error)) *fieldSpec {
	spec := &fieldSpec{
		name:     name,
		parent:   reflect.TypeFor[P](),
		result:   reflect.TypeFor[R](),
		argsType: argsType,
		pure:     pure,
	}
	for _, o := range opts {
		o(spec)
	}
	spec.calls = func(obj *objectType) (fieldCalls, error) {
		getParent, err := parentGetter[P](obj)
		if err != nil {
			return fieldCalls{}, err
		}
		return fieldCalls{
			anyResolve: func(ctx context.Context, parent, args any) (any, error) {
				return call(ctx, getParent(parent), args)
			},
			resolve: func(ctx context.Context, parent, args any, _ *FieldContext) (any, error) {
				return call(ctx, getParent(parent), args)
			},
			writeLeaf: func(leafWriter any, typ *ast.Type) func(context.Context, *jsonw.Writer, any, any, *FieldContext) error {
				lw := leafWriter.(func(*jsonw.Writer, R, *ast.Type) error)
				return func(ctx context.Context, w *jsonw.Writer, parent, args any, _ *FieldContext) error {
					v, err := call(ctx, getParent(parent), args)
					if err != nil {
						return err
					}
					return lw(w, v, typ)
				}
			},
		}, nil
	}
	return spec
}

// fieldCalls are a field's typed closures, bound to its object type.
type fieldCalls struct {
	anyResolve FieldFunc
	resolve    func(ctx context.Context, parent, args any, fc *FieldContext) (any, error)
	// writeLeaf takes the registry's typed writer for the field's leaf type
	// and shape, func(*jsonw.Writer, R, *ast.Type) error.
	writeLeaf func(leafWriter any, typ *ast.Type) func(ctx context.Context, w *jsonw.Writer, parent, args any, fc *FieldContext) error
}

// composeField binds spec to its SDL definition def on obj.
func composeField(spec *fieldSpec, b *schemaBuilder, s *Schema, obj *objectType, def *ast.FieldDefinition, fd *fieldDef) error {
	coord := coordinate(obj.name, def.Name)
	calls, err := spec.calls(obj)
	if err != nil {
		return fmt.Errorf("field %s: %w", coord, err)
	}

	if argsType := spec.argsType; argsType != nil {
		if len(def.Arguments) == 0 {
			return fmt.Errorf("field %s: binding declares arguments %s but the field has none", coord, argsType)
		}
		ab := b.reg.argsDecoders[argsType]
		if ab == nil {
			return fmt.Errorf("field %s: no Args[%s] registered", coord, argsType)
		}
		dec, err := ab.build(b, def.Arguments, coord)
		if err != nil {
			return err
		}
		fd.args = dec
	}

	fd.anyResolve = calls.anyResolve

	if isLeaf(b.ast, def.Type) {
		fd.leaf = true
		key := typeKey{def.Type.Name(), spec.result}
		if err := checkOutputLeafShape(b.reg, key, def.Type, b.ast.Types[def.Type.Name()].Kind); err != nil {
			return fmt.Errorf("field %s: %w", coord, err)
		}
		fd.writeLeaf = calls.writeLeaf(b.reg.leafWriters[key], def.Type)
		fd.writeAny = b.reg.leafWritersAny[key]
		return nil
	}

	target, err := checkOutputCompositeShape(s, spec.result, def.Type)
	if err != nil {
		return fmt.Errorf("field %s: %w", coord, err)
	}
	fd.resolve = calls.resolve
	fd.shape = b.reg.shapeFor(spec.result, def.Type, target)
	return nil
}

// parentGetter returns a typed accessor converting the executor's canonical
// *T parent value into P, which must be *T or T.
func parentGetter[P any](obj *objectType) (func(any) P, error) {
	tP := reflect.TypeFor[P]()
	switch tP {
	case obj.shapes.ptr:
		return func(v any) P { return v.(P) }, nil
	case obj.shapes.elem:
		deref := obj.shapes.deref
		return func(v any) P { return deref(v).(P) }, nil
	}
	return nil, fmt.Errorf("parent type %s does not match Object[%s] (expected %s or %s)", tP, obj.shapes.elem, obj.shapes.ptr, obj.shapes.elem)
}

// resolveFields composes every field specification of an object binding.
func (b *schemaBuilder) resolveFields(s *Schema, obj *objectType, ob *objectBinding) {
	for _, spec := range ob.fields {
		ob.attempted[spec.name] = true
		def := obj.def.Fields.ForName(spec.name)
		if def == nil {
			b.errorf("field %s is not defined in the schema", coordinate(obj.name, spec.name))
			continue
		}
		if _, dup := obj.fields[spec.name]; dup {
			b.errorf("field %s is bound more than once", coordinate(obj.name, spec.name))
			continue
		}
		fd := &fieldDef{
			name:   spec.name,
			def:    def,
			typ:    def.Type,
			object: obj,
			pure:   spec.pure,
		}
		fd.schedulable = (!spec.pure || spec.concurrent) && !spec.inline
		var err error
		if spec.compose != nil {
			err = spec.compose(b, s, obj, def, fd)
		} else {
			err = composeField(spec, b, s, obj, def, fd)
		}
		if err != nil {
			b.errs = append(b.errs, fmt.Errorf("graphql: %w", err))
			continue
		}
		obj.fields[spec.name] = fd
		if fd.schedulable {
			obj.hasSchedulable = true
		}
	}
}
