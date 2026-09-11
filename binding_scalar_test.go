package graphql

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/syssam/graphql-go/internal/jsonw"
)

func TestBuiltinScalarOutput(t *testing.T) {
	r := newTestRegistry()
	w := jsonw.New()

	if err := leafWriter[int](t, r, "Int")(w, math.MaxInt32+1, parseType("Int!")); err == nil {
		t.Fatal("Int above int32 range must fail")
	}
	w.Reset()
	if err := leafWriter[int64](t, r, "Int")(w, math.MinInt32, parseType("Int!")); err != nil || string(w.Bytes()) != "-2147483648" {
		t.Fatalf("MinInt32: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := leafWriter[float64](t, r, "Float")(w, math.Inf(1), parseType("Float!")); err == nil {
		t.Fatal("infinite Float must fail")
	}
	w.Reset()
	if err := leafWriter[float64](t, r, "Float")(w, 1.5, parseType("Float!")); err != nil || string(w.Bytes()) != "1.5" {
		t.Fatalf("Float: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := leafWriter[int64](t, r, "ID")(w, 42, parseType("ID!")); err != nil || string(w.Bytes()) != `"42"` {
		t.Fatalf("ID int64: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := leafWriter[ID](t, r, "ID")(w, "abc", parseType("ID!")); err != nil || string(w.Bytes()) != `"abc"` {
		t.Fatalf("ID: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := leafWriter[bool](t, r, "Boolean")(w, true, parseType("Boolean!")); err != nil || string(w.Bytes()) != "true" {
		t.Fatalf("Boolean: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := leafWriter[string](t, r, "String")(w, "a\"b", parseType("String!")); err != nil || string(w.Bytes()) != `"a\"b"` {
		t.Fatalf("String: %v %s", err, w.Bytes())
	}
}

func TestBuiltinScalarInput(t *testing.T) {
	r := newTestRegistry()
	nn := parseType("Int!")

	intCases := []struct {
		raw     any
		want    int
		wantErr string
	}{
		{json.Number("3"), 3, ""},
		{json.Number("3.0"), 3, ""},
		{json.Number("3.5"), 0, "non-integer"},
		{json.Number("2147483648"), 0, "32-bit"},
		{"3", 0, "non-integer"},
		{true, 0, "non-integer"},
		{float64(3), 3, ""},
		{int64(-4), -4, ""},
	}
	for _, tc := range intCases {
		got, err := decoder[int](t, r, "Int")(tc.raw, nn)
		if tc.wantErr == "" {
			if err != nil || got != tc.want {
				t.Errorf("Int(%v) = %v, %v; want %v", tc.raw, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("Int(%v) error = %v; want containing %q", tc.raw, err, tc.wantErr)
		}
	}

	if f, err := decoder[float64](t, r, "Float")(json.Number("1"), parseType("Float!")); err != nil || f != 1 {
		t.Errorf("Float(1) = %v, %v", f, err)
	}
	if _, err := decoder[float64](t, r, "Float")("1.5", parseType("Float!")); err == nil {
		t.Error("Float from string must fail")
	}
	if _, err := decoder[bool](t, r, "Boolean")("true", parseType("Boolean!")); err == nil {
		t.Error("Boolean from string must fail")
	}
	if _, err := decoder[string](t, r, "String")(json.Number("1"), parseType("String!")); err == nil {
		t.Error("String from number must fail")
	}
	if id, err := decoder[ID](t, r, "ID")(json.Number("7"), parseType("ID!")); err != nil || id != "7" {
		t.Errorf("ID(7) = %v, %v", id, err)
	}
	if _, err := decoder[ID](t, r, "ID")(json.Number("7.5"), parseType("ID!")); err == nil {
		t.Error("ID from fractional number must fail")
	}
	if n, err := decoder[int64](t, r, "ID")("12", parseType("ID!")); err != nil || n != 12 {
		t.Errorf("ID as int64 from string = %v, %v", n, err)
	}
	if _, err := decoder[int64](t, r, "ID")("x", parseType("ID!")); err == nil {
		t.Error("ID as int64 from non-numeric string must fail")
	}
}

func TestCustomScalar(t *testing.T) {
	type Money int64
	sdl := `scalar Money type Query { price: Money! prices: [Money!]! }`
	s, err := NewSchema(SDL(sdl),
		Scalar("Money", func(w *Writer, m Money) error { w.String("$" + itoa(int64(m))); return nil }, func(raw any) (Money, error) {
			n, err := rawInt64(raw, "Money")
			return Money(n), err
		}),
		Object[Root]("Query",
			Field("price", func(Root) Money { return 5 }),
			Field("prices", func(Root) []Money { return []Money{1, 2} }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	w := jsonw.New()
	if err := s.query.fields["price"].writeLeaf(ctxBackground(), w, &Root{}, nil); err != nil || string(w.Bytes()) != `"$5"` {
		t.Fatalf("price: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := s.query.fields["prices"].writeLeaf(ctxBackground(), w, &Root{}, nil); err != nil || string(w.Bytes()) != `["$1","$2"]` {
		t.Fatalf("prices: %v %s", err, w.Bytes())
	}
}

func TestScalarUnknownName(t *testing.T) {
	err := Validate(SDL(`type Query { a: Int }`),
		Object[Root]("Query", Field("a", func(Root) int { return 1 })),
		Scalar("Nope", func(*Writer, int) error { return nil }, func(any) (int, error) { return 0, nil }),
	)
	if err == nil || !strings.Contains(err.Error(), `Scalar "Nope"`) {
		t.Fatalf("expected unknown scalar error, got %v", err)
	}
}
