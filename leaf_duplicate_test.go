package graphql

import (
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
