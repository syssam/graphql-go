package graphql

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// errNonNull reports that a null value reached a non-null position. The
// executor converts it into the specification's "Cannot return null for
// non-nullable field" error and performs null bubbling.
var errNonNull = errors.New("null value for non-null type")

// indexedError attaches a list index to an error raised while writing or
// decoding a list element so that the error path can be reconstructed.
type indexedError struct {
	index int
	err   error
}

func (e *indexedError) Error() string { return fmt.Sprintf("[%d]: %v", e.index, e.err) }
func (e *indexedError) Unwrap() error { return e.err }

// typeKey identifies an adapter by the GraphQL named type it serves and the
// Go type it operates on.
type typeKey struct {
	name string
	typ  reflect.Type
}

// shapeInfo describes how a registered Go shape relates to GraphQL list
// nesting and nullability. depth is the number of list levels; nullable[i]
// reports whether level i (0 = outermost) can represent null.
type shapeInfo struct {
	depth    int
	nullable []bool
}

// registry holds the typed adapters produced by binding constructors. Values
// in leafWriters and decoders are typed function values stored as any and
// asserted back to their concrete signature by the generic code that knows V.
type registry struct {
	leafWriters    map[typeKey]any // func(*jsonw.Writer, V, *ast.Type) error
	leafWritersAny map[typeKey]func(*jsonw.Writer, any, *ast.Type) error
	decoders       map[typeKey]any // func(raw any, t *ast.Type) (V, error)
	shapes         map[typeKey]shapeInfo
	leafValidators map[string]func(any) error
	leafKinds      map[string]ast.DefinitionKind

	nilChecks  map[reflect.Type]func(any) bool
	traversers map[reflect.Type]func(any, func(int, any) bool)

	argsDecoders map[reflect.Type]*inputDecoder
	inputsByName map[string]*inputDecoder
}

func newRegistry() *registry {
	return &registry{
		leafWriters:    make(map[typeKey]any),
		leafWritersAny: make(map[typeKey]func(*jsonw.Writer, any, *ast.Type) error),
		decoders:       make(map[typeKey]any),
		shapes:         make(map[typeKey]shapeInfo),
		leafValidators: make(map[string]func(any) error),
		leafKinds:      make(map[string]ast.DefinitionKind),
		nilChecks:      make(map[reflect.Type]func(any) bool),
		traversers:     make(map[reflect.Type]func(any, func(int, any) bool)),
		argsDecoders:   make(map[reflect.Type]*inputDecoder),
		inputsByName:   make(map[string]*inputDecoder),
	}
}

func writeNull(w *jsonw.Writer, t *ast.Type) error {
	if t.NonNull {
		return errNonNull
	}
	w.Null()
	return nil
}

// registerLeaf registers writers and decoders for a scalar or enum bound to
// Go type E, in the shapes E, *E, []E and []*E.
func registerLeaf[E any](r *registry, name string, kind ast.DefinitionKind, write func(*jsonw.Writer, E) error, decode func(any) (E, error)) {
	r.leafKinds[name] = kind
	if _, ok := r.leafValidators[name]; !ok {
		r.leafValidators[name] = func(raw any) error {
			_, err := decode(raw)
			return err
		}
	}

	tE := reflect.TypeFor[E]()
	tPE := reflect.TypeFor[*E]()
	tSE := reflect.TypeFor[[]E]()
	tSPE := reflect.TypeFor[[]*E]()

	r.shapes[typeKey{name, tE}] = shapeInfo{depth: 0, nullable: []bool{false}}
	r.shapes[typeKey{name, tPE}] = shapeInfo{depth: 0, nullable: []bool{true}}
	r.shapes[typeKey{name, tSE}] = shapeInfo{depth: 1, nullable: []bool{true, false}}
	r.shapes[typeKey{name, tSPE}] = shapeInfo{depth: 1, nullable: []bool{true, true}}

	wE := func(w *jsonw.Writer, v E, _ *ast.Type) error { return write(w, v) }
	wPE := func(w *jsonw.Writer, v *E, t *ast.Type) error {
		if v == nil {
			return writeNull(w, t)
		}
		return write(w, *v)
	}
	// Element failures inside a list with nullable elements null out only that
	// element; the collected errors are returned as elementErrors after the
	// list has been written in full.
	wSE := func(w *jsonw.Writer, v []E, t *ast.Type) error {
		if v == nil {
			return writeNull(w, t)
		}
		var soft *elementErrors
		w.BeginArray()
		for i, e := range v {
			m := w.Mark()
			if err := write(w, e); err != nil {
				if t.Elem.NonNull {
					return &indexedError{i, err}
				}
				w.Rewind(m)
				w.Null()
				soft = soft.add(i, err)
			}
		}
		w.EndArray()
		if soft != nil {
			return soft
		}
		return nil
	}
	wSPE := func(w *jsonw.Writer, v []*E, t *ast.Type) error {
		if v == nil {
			return writeNull(w, t)
		}
		var soft *elementErrors
		w.BeginArray()
		for i, e := range v {
			if e == nil {
				if t.Elem.NonNull {
					return &indexedError{i, errNonNull}
				}
				w.Null()
				continue
			}
			m := w.Mark()
			if err := write(w, *e); err != nil {
				if t.Elem.NonNull {
					return &indexedError{i, err}
				}
				w.Rewind(m)
				w.Null()
				soft = soft.add(i, err)
			}
		}
		w.EndArray()
		if soft != nil {
			return soft
		}
		return nil
	}
	setLeafWriter(r, name, tE, wE)
	setLeafWriter(r, name, tPE, wPE)
	setLeafWriter(r, name, tSE, wSE)
	setLeafWriter(r, name, tSPE, wSPE)

	dE := func(raw any, _ *ast.Type) (E, error) {
		if raw == nil {
			var zero E
			return zero, errNonNull
		}
		return decode(raw)
	}
	dPE := func(raw any, _ *ast.Type) (*E, error) {
		if raw == nil {
			return nil, nil
		}
		v, err := decode(raw)
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	dSE := func(raw any, t *ast.Type) ([]E, error) {
		if raw == nil {
			return nil, nil
		}
		items := asList(raw)
		out := make([]E, len(items))
		for i, it := range items {
			if it == nil {
				return nil, &indexedError{i, errNonNull}
			}
			v, err := decode(it)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out[i] = v
		}
		return out, nil
	}
	dSPE := func(raw any, t *ast.Type) ([]*E, error) {
		if raw == nil {
			return nil, nil
		}
		items := asList(raw)
		out := make([]*E, len(items))
		for i, it := range items {
			if it == nil {
				if t.Elem.NonNull {
					return nil, &indexedError{i, errNonNull}
				}
				continue
			}
			v, err := decode(it)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out[i] = &v
		}
		return out, nil
	}
	r.decoders[typeKey{name, tE}] = dE
	r.decoders[typeKey{name, tPE}] = dPE
	r.decoders[typeKey{name, tSE}] = dSE
	r.decoders[typeKey{name, tSPE}] = dSPE

	r.nilChecks[tPE] = func(v any) bool { p, _ := v.(*E); return p == nil }
	r.nilChecks[tSE] = func(v any) bool { s, _ := v.([]E); return s == nil }
	r.nilChecks[tSPE] = func(v any) bool { s, _ := v.([]*E); return s == nil }
}

func setLeafWriter[V any](r *registry, name string, t reflect.Type, typed func(*jsonw.Writer, V, *ast.Type) error) {
	r.leafWriters[typeKey{name, t}] = typed
	r.leafWritersAny[typeKey{name, t}] = func(w *jsonw.Writer, v any, at *ast.Type) error {
		tv, ok := v.(V)
		if !ok {
			if v == nil {
				return writeNull(w, at)
			}
			return fmt.Errorf("cannot marshal %T as %s", v, name)
		}
		return typed(w, tv, at)
	}
}

// asList applies the specification's list input coercion: a non-list value
// is treated as a single-element list.
func asList(raw any) []any {
	if items, ok := raw.([]any); ok {
		return items
	}
	return []any{raw}
}

// objectShapes holds the typed adapters an object type contributes to the
// registry. E is the non-pointer Go type named in Object[E]; values flow
// through the executor as *E.
type objectShapes struct {
	elem  reflect.Type // E
	ptr   reflect.Type // *E
	toPtr func(any) any // E → *E (copies the value)
	deref func(any) any // *E → E
}

// registerObjectShapes registers nil checks, traversers and conversions for
// an object type bound to Go type E.
func registerObjectShapes[E any](r *registry) objectShapes {
	tE := reflect.TypeFor[E]()
	tPE := reflect.TypeFor[*E]()
	tSE := reflect.TypeFor[[]E]()
	tSPE := reflect.TypeFor[[]*E]()
	r.nilChecks[tPE] = func(v any) bool { p, _ := v.(*E); return p == nil }
	r.nilChecks[tSE] = func(v any) bool { s, _ := v.([]E); return s == nil }
	r.nilChecks[tSPE] = func(v any) bool { s, _ := v.([]*E); return s == nil }
	r.traversers[tSE] = func(v any, yield func(int, any) bool) {
		for i, e := range v.([]E) {
			if !yield(i, e) {
				return
			}
		}
	}
	r.traversers[tSPE] = func(v any, yield func(int, any) bool) {
		for i, e := range v.([]*E) {
			if !yield(i, e) {
				return
			}
		}
	}
	return objectShapes{
		elem:  tE,
		ptr:   tPE,
		toPtr: func(v any) any { e := v.(E); return &e },
		deref: func(v any) any { return *(v.(*E)) },
	}
}

// registerAbstractShapes registers nil checks and traversers for an abstract
// type bound to Go type T (typically an interface type or any).
func registerAbstractShapes[T any](r *registry) {
	tT := reflect.TypeFor[T]()
	tST := reflect.TypeFor[[]T]()
	if _, ok := r.nilChecks[tT]; !ok {
		r.nilChecks[tT] = func(v any) bool { return v == nil }
	}
	r.nilChecks[tST] = func(v any) bool { s, _ := v.([]T); return s == nil }
	r.traversers[tST] = func(v any, yield func(int, any) bool) {
		for i, e := range v.([]T) {
			if !yield(i, e) {
				return
			}
		}
	}
}

// valueShape describes how the executor inspects a composite result whose
// static Go type is known at schema build time. toPtr is set when the named
// level carries a value E that must become *E before field access.
type valueShape struct {
	isNil    func(any) bool
	traverse func(any, func(int, any) bool)
	toPtr    func(any) any
	elem     *valueShape
}

var reflectionWarned sync.Map

// shapeFor derives the value shape for Go type t matched against SDL type
// sdl. Object bindings supply typed traversers; unknown slice types fall
// back to reflection with a one-time warning. obj is the object type at the
// named level, or nil for abstract types.
func (r *registry) shapeFor(t reflect.Type, sdl *ast.Type, obj *objectType) *valueShape {
	s := &valueShape{isNil: r.nilChecks[t]}
	if s.isNil == nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Interface, reflect.Map:
			s.isNil = reflectIsNil
		}
	}
	if sdl.Elem != nil {
		if tr, ok := r.traversers[t]; ok {
			s.traverse = tr
		} else {
			if _, warned := reflectionWarned.LoadOrStore(t, true); !warned {
				slog.Warn("graphql: using reflection to traverse list type; register the shape through Object or Interface for a typed loop", "type", t.String())
			}
			s.traverse = reflectTraverse
		}
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			s.elem = r.shapeFor(t.Elem(), sdl.Elem, obj)
		} else {
			s.elem = &valueShape{isNil: reflectIsNil}
		}
		return s
	}
	if obj != nil && t == obj.shapes.elem {
		s.toPtr = obj.shapes.toPtr
	}
	return s
}

func reflectIsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

func reflectTraverse(v any, yield func(int, any) bool) {
	rv := reflect.ValueOf(v)
	for i := range rv.Len() {
		if !yield(i, rv.Index(i).Interface()) {
			return
		}
	}
}
