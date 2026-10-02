package graphql

import (
	"errors"
	"fmt"
	"iter"
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
// decoders holds leaf shapes only: an input object's shapes are in
// decodersAny alone, and inputDecoderFor adapts them for an InputField.
type registry struct {
	leafWriters    map[typeKey]any // func(*jsonw.Writer, V, *ast.Type) error
	leafWritersAny map[typeKey]func(*jsonw.Writer, any, *ast.Type) error
	decoders       map[typeKey]any // func(raw any, t *ast.Type) (V, error)
	decodersAny    map[typeKey]func(any, *ast.Type) (any, error)
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
		decodersAny:    make(map[typeKey]func(any, *ast.Type) (any, error)),
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

// nullInput reports a null input value against its SDL type. Validation
// rejects nulls in non-null positions before decoding; this is the last line
// of defence.
func nullInput(t *ast.Type) error {
	if t.NonNull {
		return errNonNull
	}
	return nil
}

// registerLeaf registers writers and decoders for a scalar or enum bound to
// Go type E, in the shapes E, *E, []E, []*E, [][]E and [][]*E. Deeper list
// nesting is rare enough that it is left unsupported for leaf types.
//
// Only the typed closures are generic; the registry bookkeeping is
// addLeafShape, and a list element's failure is listElementFailed. Generated
// code binds hundreds of leaf types, each instantiating this in its own
// package, so what is not generic here is compiled once instead of each time.
func registerLeaf[E any](r *registry, name string, kind ast.DefinitionKind, write func(*jsonw.Writer, E) error, decode func(any) (E, error)) {
	r.leafKinds[name] = kind
	if _, ok := r.leafValidators[name]; !ok {
		r.leafValidators[name] = func(raw any) error {
			_, err := decode(raw)
			return err
		}
	}

	wE := func(w *jsonw.Writer, v E, _ *ast.Type) error { return write(w, v) }
	wPE := func(w *jsonw.Writer, v *E, t *ast.Type) error {
		if v == nil {
			return writeNull(w, t)
		}
		return write(w, *v)
	}
	dE := func(raw any, _ *ast.Type) (E, error) {
		if raw == nil {
			var zero E
			return zero, errNonNull
		}
		return decode(raw)
	}
	dPE := func(raw any, t *ast.Type) (*E, error) {
		if raw == nil {
			return nil, nullInput(t)
		}
		v, err := decode(raw)
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	// A Go interface holds nil, so as E itself it can stand for null: a value of type any backs
	// a nullable JSON argument without a pointer to the interface. Its decoder therefore maps
	// null to the zero value instead of refusing it; a NON-NULL SDL position still refuses null
	// before any decoder runs, so nothing is loosened there.
	eNilable := reflect.TypeFor[E]().Kind() == reflect.Interface
	if eNilable {
		dE = func(raw any, _ *ast.Type) (E, error) {
			if raw == nil {
				var zero E
				return zero, nil
			}
			return decode(raw)
		}
	}
	wSE, dSE := listWriter(wE), listDecoder(dE)
	wSPE, dSPE := listWriter(wPE), listDecoder(dPE)

	r.addLeafShape(name, []bool{eNilable}, leafShape(name, wE, dE))
	r.addLeafShape(name, []bool{true}, leafShape(name, wPE, dPE))
	r.addLeafShape(name, []bool{true, eNilable}, leafShape(name, wSE, dSE))
	r.addLeafShape(name, []bool{true, true}, leafShape(name, wSPE, dSPE))
	r.addLeafShape(name, []bool{true, true, eNilable}, leafShape(name, listWriter(wSE), listDecoder(dSE)))
	r.addLeafShape(name, []bool{true, true, true}, leafShape(name, listWriter(wSPE), listDecoder(dSPE)))
}

// claimLeaf records that an option binds the leaf type name to Go type t, and
// refuses a second binding of the same pair. Several Go types may back one
// GraphQL type, so the pair is the key; without the check the later option
// replaced the earlier one's writer and decoders while the first kept
// validating variables, and which of two bindings answered depended on the
// order of the options. It is not generic so that the binding functions, which
// are instantiated once per type in generated code, only call it.
func (b *schemaBuilder) claimLeaf(option, name string, t reflect.Type) bool {
	key := typeKey{name: name, typ: t}
	if b.leafBound[key] {
		b.errorf("%s %q: Go type %s is bound more than once", option, name, t)
		return false
	}
	b.leafBound[key] = true
	return true
}

// leafAdapters are one Go shape V of a leaf type: its typed writer and
// decoder, stored as any, and the type-erased forms directives and variable
// coercion use.
type leafAdapters struct {
	typ       reflect.Type
	write     any // func(*jsonw.Writer, V, *ast.Type) error
	writeAny  func(*jsonw.Writer, any, *ast.Type) error
	decode    any // func(any, *ast.Type) (V, error)
	decodeAny func(any, *ast.Type) (any, error)
}

func leafShape[V any](name string, write func(*jsonw.Writer, V, *ast.Type) error, decode func(any, *ast.Type) (V, error)) leafAdapters {
	return leafAdapters{
		typ:   reflect.TypeFor[V](),
		write: write,
		writeAny: func(w *jsonw.Writer, v any, at *ast.Type) error {
			tv, ok := v.(V)
			if !ok {
				return writeMismatch(w, v, at, name)
			}
			return write(w, tv, at)
		},
		decode:    decode,
		decodeAny: erased(decode),
	}
}

func writeMismatch(w *jsonw.Writer, v any, at *ast.Type, name string) error {
	if v == nil {
		return writeNull(w, at)
	}
	return fmt.Errorf("cannot marshal %T as %s", v, name)
}

// addLeafShape records one Go shape of a leaf type. nullable has one entry
// per list level plus the innermost value, outermost first.
func (r *registry) addLeafShape(name string, nullable []bool, a leafAdapters) {
	key := typeKey{name, a.typ}
	r.shapes[key] = shapeInfo{depth: len(nullable) - 1, nullable: nullable}
	r.leafWriters[key] = a.write
	r.leafWritersAny[key] = a.writeAny
	r.decoders[key] = a.decode
	r.decodersAny[key] = a.decodeAny
	if nullable[0] {
		// Every nullable shape is a pointer or a slice.
		r.nilChecks[a.typ] = reflectIsNil
	}
}

// listWriter lifts an element writer to a slice writer. Element failures
// inside a list with nullable elements null out only that element; the
// collected errors are returned as elementErrors after the list has been
// written in full. Failures under a non-null element type abort the list.
func listWriter[V any](elem func(*jsonw.Writer, V, *ast.Type) error) func(*jsonw.Writer, []V, *ast.Type) error {
	return func(w *jsonw.Writer, v []V, t *ast.Type) error {
		if v == nil {
			return writeNilList(w, t)
		}
		var soft *elementErrors
		w.BeginArray()
		for i, e := range v {
			m := w.Mark()
			if err := elem(w, e, t.Elem); err != nil {
				if soft, err = listElementFailed(w, m, soft, i, err, t); err != nil {
					return err
				}
			}
		}
		w.EndArray()
		if soft != nil {
			return soft
		}
		return nil
	}
}

func writeNilList(w *jsonw.Writer, t *ast.Type) error {
	if t.NonNull {
		// See valueShape.nilIsEmpty: a nil slice is Go's empty list.
		w.BeginArray()
		w.EndArray()
		return nil
	}
	return writeNull(w, t)
}

// listElementFailed handles element i of a list of type t failing with err
// after it began writing at m. It returns the list's collected soft errors,
// or the error that aborts the list.
func listElementFailed(w *jsonw.Writer, m jsonw.Mark, soft *elementErrors, i int, err error, t *ast.Type) (*elementErrors, error) {
	var nested *elementErrors
	if errors.As(err, &nested) {
		// The element is a list that already nulled its own failing
		// members; keep it and re-index the collected errors.
		for _, ie := range nested.errs {
			soft = soft.add(i, ie)
		}
		return soft, nil
	}
	if t.Elem.NonNull {
		return soft, &indexedError{i, err}
	}
	w.Rewind(m)
	w.Null()
	return soft.add(i, err), nil
}

// listDecoder lifts an element decoder to a slice decoder applying the
// specification's list coercion; a non-list input becomes a one-element list.
func listDecoder[V any](elem func(any, *ast.Type) (V, error)) func(any, *ast.Type) ([]V, error) {
	return func(raw any, t *ast.Type) ([]V, error) {
		if raw == nil {
			return nil, nullInput(t)
		}
		items := asList(raw)
		out := make([]V, len(items))
		for i, it := range items {
			v, err := elem(it, t.Elem)
			if err != nil {
				return nil, &indexedError{i, err}
			}
			out[i] = v
		}
		return out, nil
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
	elem  reflect.Type  // E
	ptr   reflect.Type  // *E
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
	tQE := reflect.TypeFor[iter.Seq[E]]()
	tQPE := reflect.TypeFor[iter.Seq[*E]]()
	r.nilChecks[tQE] = func(v any) bool { q, _ := v.(iter.Seq[E]); return q == nil }
	r.nilChecks[tQPE] = func(v any) bool { q, _ := v.(iter.Seq[*E]); return q == nil }
	// The executor's traverser contract carries an index for error paths and
	// a seq does not, so it is counted here.
	r.traversers[tQE] = func(v any, yield func(int, any) bool) {
		i := 0
		for e := range v.(iter.Seq[E]) {
			if !yield(i, e) {
				return
			}
			i++
		}
	}
	r.traversers[tQPE] = func(v any, yield func(int, any) bool) {
		i := 0
		for e := range v.(iter.Seq[*E]) {
			if !yield(i, e) {
				return
			}
			i++
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
	tQT := reflect.TypeFor[iter.Seq[T]]()
	r.nilChecks[tQT] = func(v any) bool { q, _ := v.(iter.Seq[T]); return q == nil }
	// See registerObjectShapes: the seq has no length, so the index is counted
	// here to satisfy the traverser contract's error-path requirement.
	r.traversers[tQT] = func(v any, yield func(int, any) bool) {
		i := 0
		for e := range v.(iter.Seq[T]) {
			if !yield(i, e) {
				return
			}
			i++
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
	// nilIsEmpty is set for a Go slice at a non-null list position, where a
	// nil slice is written as [] rather than as a null violation: null is not
	// a valid answer there, and a nil slice is Go's empty list. Decided here,
	// at plan time, so the executor tests a bool and reflects on nothing.
	nilIsEmpty bool
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
		case reflect.Pointer, reflect.Slice, reflect.Interface, reflect.Map, reflect.Func:
			s.isNil = reflectIsNil
		}
	}
	if sdl.Elem != nil {
		s.nilIsEmpty = sdl.NonNull && t.Kind() == reflect.Slice
		if tr, ok := r.traversers[t]; ok {
			s.traverse = tr
		} else {
			if _, warned := reflectionWarned.LoadOrStore(t, true); !warned {
				slog.Warn("graphql: using reflection to traverse list type; register the shape through Object or Interface for a typed loop", "type", t.String())
			}
			s.traverse = reflectTraverse
		}
		switch e, isSeq := seqElem(t); {
		case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
			s.elem = r.shapeFor(t.Elem(), sdl.Elem, obj)
		case isSeq:
			s.elem = r.shapeFor(e, sdl.Elem, obj)
		default:
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

// seqElem reports the element type of an iter.Seq-shaped function type,
// func(yield func(E) bool). Matching is structural rather than on iter's
// package path: a caller's own equivalent behaves identically and there is
// no reason to reject it. Schema build time only.
func seqElem(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() != reflect.Func || t.IsVariadic() || t.NumIn() != 1 || t.NumOut() != 0 {
		return nil, false
	}
	y := t.In(0)
	if y.Kind() != reflect.Func || y.IsVariadic() || y.NumIn() != 1 || y.NumOut() != 1 {
		return nil, false
	}
	if y.Out(0).Kind() != reflect.Bool {
		return nil, false
	}
	return y.In(0), true
}

func reflectTraverse(v any, yield func(int, any) bool) {
	rv := reflect.ValueOf(v)
	for i := range rv.Len() {
		if !yield(i, rv.Index(i).Interface()) {
			return
		}
	}
}
