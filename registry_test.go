package graphql

import (
	"errors"
	"reflect"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

func ptr[T any](v T) *T { return &v }

// parseType builds an ast.Type from a compact SDL type string.
func parseType(s string) *ast.Type {
	nonNull := false
	if s[len(s)-1] == '!' {
		nonNull = true
		s = s[:len(s)-1]
	}
	if s[0] == '[' {
		return &ast.Type{Elem: parseType(s[1 : len(s)-1]), NonNull: nonNull}
	}
	return &ast.Type{NamedType: s, NonNull: nonNull}
}

func leafWriter[V any](t *testing.T, r *registry, name string) func(*jsonw.Writer, V, *ast.Type) error {
	t.Helper()
	lw, ok := r.leafWriters[typeKey{name, reflect.TypeFor[V]()}]
	if !ok {
		t.Fatalf("no leaf writer for %s/%s", name, reflect.TypeFor[V]())
	}
	return lw.(func(*jsonw.Writer, V, *ast.Type) error)
}

func decoder[V any](t *testing.T, r *registry, name string) func(any, *ast.Type) (V, error) {
	t.Helper()
	d, ok := r.decoders[typeKey{name, reflect.TypeFor[V]()}]
	if !ok {
		t.Fatalf("no decoder for %s/%s", name, reflect.TypeFor[V]())
	}
	return d.(func(any, *ast.Type) (V, error))
}

func newTestRegistry() *registry {
	b := &schemaBuilder{reg: newRegistry()}
	registerBuiltins(b)
	return b.reg
}

func TestLeafWriterShapes(t *testing.T) {
	r := newTestRegistry()
	w := jsonw.New()

	if err := leafWriter[int](t, r, "Int")(w, 7, parseType("Int!")); err != nil || string(w.Bytes()) != "7" {
		t.Fatalf("int: %v %s", err, w.Bytes())
	}

	w.Reset()
	if err := leafWriter[*int](t, r, "Int")(w, nil, parseType("Int!")); !errors.Is(err, errNonNull) {
		t.Fatalf("nil *int for Int! should be errNonNull, got %v", err)
	}

	w.Reset()
	if err := leafWriter[*int](t, r, "Int")(w, nil, parseType("Int")); err != nil || string(w.Bytes()) != "null" {
		t.Fatalf("nil *int for Int: %v %s", err, w.Bytes())
	}

	w.Reset()
	if err := leafWriter[[]int](t, r, "Int")(w, []int{1, 2}, parseType("[Int]")); err != nil || string(w.Bytes()) != "[1,2]" {
		t.Fatalf("[]int: %v %s", err, w.Bytes())
	}

	w.Reset()
	err := leafWriter[[]*int](t, r, "Int")(w, []*int{ptr(1), nil}, parseType("[Int!]!"))
	var ie *indexedError
	if !errors.As(err, &ie) || ie.index != 1 || !errors.Is(err, errNonNull) {
		t.Fatalf("nil element in [Int!]! should be indexedError{1, errNonNull}, got %v", err)
	}

	w.Reset()
	if err := leafWriter[[]*int](t, r, "Int")(w, []*int{ptr(1), nil}, parseType("[Int]!")); err != nil || string(w.Bytes()) != "[1,null]" {
		t.Fatalf("[Int]! with nil element: %v %s", err, w.Bytes())
	}

	w.Reset()
	if err := leafWriter[[]int](t, r, "Int")(w, nil, parseType("[Int!]!")); !errors.Is(err, errNonNull) {
		t.Fatalf("nil slice for [Int!]! should be errNonNull, got %v", err)
	}
}

func TestNestedLeafListShapes(t *testing.T) {
	r := newTestRegistry()
	w := jsonw.New()

	if err := leafWriter[[][]int](t, r, "Int")(w, [][]int{{1, 2}, {}}, parseType("[[Int!]!]!")); err != nil || string(w.Bytes()) != "[[1,2],[]]" {
		t.Fatalf("[][]int: %v %s", err, w.Bytes())
	}

	w.Reset()
	if err := leafWriter[[][]*int](t, r, "Int")(w, [][]*int{{ptr(1), nil}, nil}, parseType("[[Int]]")); err != nil || string(w.Bytes()) != "[[1,null],null]" {
		t.Fatalf("[][]*int with nulls: %v %s", err, w.Bytes())
	}

	// A range error two levels deep must null only that element and report
	// the full index chain.
	w.Reset()
	err := leafWriter[[][]int64](t, r, "Int")(w, [][]int64{{1}, {1 << 40, 2}}, parseType("[[Int]!]!"))
	var soft *elementErrors
	if !errors.As(err, &soft) || len(soft.errs) != 1 || string(w.Bytes()) != "[[1],[null,2]]" {
		t.Fatalf("nested soft error: %v %s", err, w.Bytes())
	}
	var outer *indexedError
	if !errors.As(soft.errs[0], &outer) || outer.index != 1 {
		t.Fatalf("outer index: %v", soft.errs[0])
	}
	var inner *indexedError
	if !errors.As(outer.err, &inner) || inner.index != 0 {
		t.Fatalf("inner index: %v", outer.err)
	}

	// A non-null inner element failure nulls the nullable inner list.
	w.Reset()
	err = leafWriter[[][]int64](t, r, "Int")(w, [][]int64{{1 << 40}, {5}}, parseType("[[Int!]]"))
	if !errors.As(err, &soft) || len(soft.errs) != 1 || string(w.Bytes()) != "[null,[5]]" {
		t.Fatalf("nullable inner list: %v %s", err, w.Bytes())
	}

	// When the inner list is non-null too, the failure aborts the whole list.
	w.Reset()
	err = leafWriter[[][]int64](t, r, "Int")(w, [][]int64{{1 << 40}}, parseType("[[Int!]!]"))
	if !errors.As(err, &outer) || outer.index != 0 || !errors.As(outer.err, &inner) || inner.index != 0 {
		t.Fatalf("non-null nested element error: %v", err)
	}

	if v, err := decoder[[][]*int](t, r, "Int")([]any{[]any{int64(1), nil}, nil}, parseType("[[Int]]")); err != nil || len(v) != 2 || *v[0][0] != 1 || v[0][1] != nil || v[1] != nil {
		t.Fatalf("decode [][]*int: %v %v", v, err)
	}
	if _, err := decoder[[][]int](t, r, "Int")([]any{[]any{int64(1)}, nil}, parseType("[[Int!]!]")); !errors.Is(err, errNonNull) {
		t.Fatalf("null inner list for [[Int!]!] should fail, got %v", err)
	}
	if !r.nilChecks[reflect.TypeFor[[][]int]()]([][]int(nil)) || r.nilChecks[reflect.TypeFor[[][]int]()]([][]int{}) {
		t.Fatal("nil check for [][]int")
	}
}

func TestLeafDecoderShapes(t *testing.T) {
	r := newTestRegistry()

	if v, err := decoder[int](t, r, "Int")(int64(3), parseType("Int!")); err != nil || v != 3 {
		t.Fatalf("int: %v %v", v, err)
	}
	if _, err := decoder[int](t, r, "Int")(nil, parseType("Int!")); !errors.Is(err, errNonNull) {
		t.Fatalf("nil into int should fail, got %v", err)
	}
	if v, err := decoder[*int](t, r, "Int")(nil, parseType("Int")); err != nil || v != nil {
		t.Fatalf("nil into *int: %v %v", v, err)
	}
	if v, err := decoder[[]int](t, r, "Int")(int64(5), parseType("[Int]")); err != nil || len(v) != 1 || v[0] != 5 {
		t.Fatalf("single value list coercion: %v %v", v, err)
	}
	if v, err := decoder[[]*int](t, r, "Int")([]any{int64(1), nil}, parseType("[Int]")); err != nil || len(v) != 2 || *v[0] != 1 || v[1] != nil {
		t.Fatalf("[]*int with null element: %v %v", v, err)
	}
	if _, err := decoder[[]*int](t, r, "Int")([]any{int64(1), nil}, parseType("[Int!]")); !errors.Is(err, errNonNull) {
		t.Fatalf("null element in [Int!] should fail, got %v", err)
	}
}

func TestNilChecks(t *testing.T) {
	r := newTestRegistry()
	var np *int
	if !r.nilChecks[reflect.TypeFor[*int]()](np) {
		t.Fatal("nil *int must be detected")
	}
	if r.nilChecks[reflect.TypeFor[*int]()](ptr(1)) {
		t.Fatal("non-nil *int must not be detected as nil")
	}
	if r.nilChecks[reflect.TypeFor[int]()] != nil {
		t.Fatal("value types never register a nil check")
	}
}

func TestObjectShapes(t *testing.T) {
	type U struct{ N int }
	r := newRegistry()
	sh := registerObjectShapes[U](r)
	if sh.elem != reflect.TypeFor[U]() || sh.ptr != reflect.TypeFor[*U]() {
		t.Fatal("unexpected shape types")
	}
	var got []any
	r.traversers[reflect.TypeFor[[]*U]()]([]*U{{1}, {2}}, func(i int, v any) bool {
		got = append(got, v.(*U).N)
		return true
	})
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("traverser yielded %v", got)
	}
	p := sh.toPtr(U{3}).(*U)
	if p.N != 3 {
		t.Fatal("toPtr failed")
	}
	if sh.deref(p).(U).N != 3 {
		t.Fatal("deref failed")
	}
	if !r.nilChecks[reflect.TypeFor[[]*U]()]([]*U(nil)) {
		t.Fatal("nil slice must be detected")
	}
}

func TestShapeForReflectionFallback(t *testing.T) {
	type U struct{ N int }
	r := newRegistry()
	registerObjectShapes[U](r)
	sh := r.shapeFor(reflect.TypeFor[[][]*U](), parseType("[[U]]"), nil)
	if sh.traverse == nil || sh.elem == nil || sh.elem.traverse == nil {
		t.Fatal("nested shape incomplete")
	}
	var n int
	sh.traverse([][]*U{{{1}}, {{2}, {3}}}, func(_ int, inner any) bool {
		sh.elem.traverse(inner, func(_ int, v any) bool {
			n += v.(*U).N
			return true
		})
		return true
	})
	if n != 6 {
		t.Fatalf("sum = %d, want 6", n)
	}
}
