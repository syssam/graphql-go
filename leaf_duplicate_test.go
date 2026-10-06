package graphql

import (
	"fmt"
	"strings"
	"testing"
)

type dupDate string

type dupColor int

// A second Scalar or Enum for the same GraphQL type and the same Go type
// replaced the first without a word, while every other binding refuses a
// duplicate. Which one answered then depended on option order, which
// NewSchema promises never matters.
func TestBindingALeafTwiceForOneGoTypeIsABuildError(t *testing.T) {
	scalar := func(tag string) SchemaOption {
		return Scalar("Date",
			func(w *Writer, d dupDate) error { w.String(tag + string(d)); return nil },
			func(v any) (dupDate, error) { return dupDate(tag), nil })
	}
	_, err := NewSchema(SDL(`scalar Date enum Color { RED } type Query { d: Date c: Color }`),
		scalar("first:"), scalar("second:"),
		Enum("Color", map[dupColor]string{0: "RED"}),
		Enum("Color", map[dupColor]string{1: "RED"}),
		Query(
			Field("d", func(Root) *dupDate { return nil }),
			Field("c", func(Root) *dupColor { return nil }),
		),
	)
	if err == nil {
		t.Fatal("two bindings of one leaf type to one Go type were accepted; the second silently replaced the first")
	}
	for _, want := range []string{`Scalar "Date"`, `Enum "Color"`, "dupDate", "dupColor", "bound more than once"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// The built-in scalars bind their Go types before any option runs, and those
// pairs are taken like any other. Scalar[string]("String", ...) used to build
// and replace how every String in the schema was written and decoded,
// introspection's own included, while variables went on being validated by
// the built-in decoder.
func TestRebindingABuiltInScalarsOwnGoTypeIsABuildError(t *testing.T) {
	upper := func(w *Writer, v string) error { w.String(strings.ToUpper(v)); return nil }
	_, err := NewSchema(SDL(`type Query { a: String b: ID c: Int d: Float e: Boolean }`),
		Scalar("String", upper, func(v any) (string, error) { return "", nil }),
		Scalar("ID", func(w *Writer, v int64) error { w.Int64(v); return nil }, func(v any) (int64, error) { return 0, nil }),
		Scalar("Int", func(w *Writer, v int64) error { w.Int64(v); return nil }, func(v any) (int64, error) { return 0, nil }),
		Scalar("Float", func(w *Writer, v float32) error { return w.Float64(float64(v)) }, func(v any) (float32, error) { return 0, nil }),
		Scalar("Boolean", func(w *Writer, v bool) error { w.Bool(v); return nil }, func(v any) (bool, error) { return false, nil }),
		Query(
			Field("a", func(Root) *string { return nil }),
			Field("b", func(Root) *ID { return nil }),
			Field("c", func(Root) *int { return nil }),
			Field("d", func(Root) *float64 { return nil }),
			Field("e", func(Root) *bool { return nil }),
		),
	)
	if err == nil {
		t.Fatal("a built-in scalar's own Go type was bound again; the option silently replaced the built-in")
	}
	for _, want := range []string{
		`Scalar "String": the built-in String scalar already binds Go type string`,
		`Scalar "ID": the built-in ID scalar already binds Go type int64`,
		`Scalar "Int": the built-in Int scalar already binds Go type int64`,
		`Scalar "Float": the built-in Float scalar already binds Go type float32`,
		`Scalar "Boolean": the built-in Boolean scalar already binds Go type bool`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%v", want, err)
		}
	}
}

// A Go type the built-in does not bind is added beside the ones it does, which
// is how ID becomes a UUID type without the string and integer IDs changing.
func TestBindingAnotherGoTypeToABuiltInScalarAddsToIt(t *testing.T) {
	type uuid [2]byte
	s, err := NewSchema(SDL(`type Query { u: ID! s: ID! n: ID! echo(id: ID!): ID! }`),
		Scalar("ID", func(w *Writer, v uuid) error { w.String(fmt.Sprintf("%x", v[:])); return nil },
			func(v any) (uuid, error) {
				s, _ := v.(string)
				var u uuid
				if _, err := fmt.Sscanf(s, "%02x%02x", &u[0], &u[1]); err != nil {
					return u, fmt.Errorf("%q is not a uuid", s)
				}
				return u, nil
			}),
		Query(
			Field("u", func(Root) uuid { return uuid{0xab, 0xcd} }),
			Field("s", func(Root) string { return "plain" }),
			Field("n", func(Root) int64 { return 7 }),
			FieldArgs("echo", func(_ Root, a struct{ ID uuid }) uuid { return a.ID }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ u s n echo(id: "0102") }`, ""), `{"u":"abcd","s":"plain","n":"7","echo":"0102"}`)
}

// Several Go types behind one GraphQL type is a feature and stays one.
func TestBindingALeafToSeveralGoTypesStillBuilds(t *testing.T) {
	type other string
	_, err := NewSchema(SDL(`scalar Date type Query { a: Date b: Date }`),
		Scalar("Date", func(w *Writer, d dupDate) error { w.String(string(d)); return nil },
			func(v any) (dupDate, error) { return "", nil }),
		Scalar("Date", func(w *Writer, d other) error { w.String(string(d)); return nil },
			func(v any) (other, error) { return "", nil }),
		Query(
			Field("a", func(Root) *dupDate { return nil }),
			Field("b", func(Root) *other { return nil }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
}

// float32 was widened to float64 and printed with float64's digits, so 0.1
// went out as 0.10000000149011612.
func TestFloat32PrintsItsOwnShortestDigits(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { f: Float! big: Float! }`), Query(
		Field("f", func(Root) float32 { return 0.1 }),
		Field("big", func(Root) float32 { return 1e22 }),
	))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ f big }`, ""), `{"f":0.1,"big":1e+22}`)
}
