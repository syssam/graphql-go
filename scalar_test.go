package graphql

import (
	"context"
	"encoding/json"
	"fmt"
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
	if err := s.query.fields["price"].writeLeaf(ctxBackground(), w, &Root{}, nil, nil); err != nil || string(w.Bytes()) != `"$5"` {
		t.Fatalf("price: %v %s", err, w.Bytes())
	}
	w.Reset()
	if err := s.query.fields["prices"].writeLeaf(ctxBackground(), w, &Root{}, nil, nil); err != nil || string(w.Bytes()) != `["$1","$2"]` {
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

// --- built-in scalar input coercion -------------------------------------

type coIntArgs struct{ V int }
type coFloatArgs struct{ V float64 }
type coIDStrArgs struct{ V ID }
type coIDIntArgs struct{ V int64 }
type coStrArgs struct{ V string }
type coBoolArgs struct{ V bool }

func newScalarInputExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
		type Query {
			i(v: Int!): String!
			f(v: Float!): String!
			ids(v: ID!): String!
			idi(v: ID!): String!
			s(v: String!): String!
			b(v: Boolean!): String!
		}
	`),
		Args[coIntArgs](), Args[coFloatArgs](), Args[coIDStrArgs](),
		Args[coIDIntArgs](), Args[coStrArgs](), Args[coBoolArgs](),
		Query(
			ResolveArgs("i", func(_ context.Context, _ Root, a coIntArgs) (string, error) {
				return fmt.Sprint(a.V), nil
			}),
			ResolveArgs("f", func(_ context.Context, _ Root, a coFloatArgs) (string, error) {
				return fmt.Sprint(a.V), nil
			}),
			ResolveArgs("ids", func(_ context.Context, _ Root, a coIDStrArgs) (string, error) {
				return string(a.V), nil
			}),
			ResolveArgs("idi", func(_ context.Context, _ Root, a coIDIntArgs) (string, error) {
				return fmt.Sprint(a.V), nil
			}),
			ResolveArgs("s", func(_ context.Context, _ Root, a coStrArgs) (string, error) {
				return a.V, nil
			}),
			ResolveArgs("b", func(_ context.Context, _ Root, a coBoolArgs) (string, error) {
				return fmt.Sprint(a.V), nil
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

// TestScalarInputCoercion covers the built-in scalar decoders against the
// specification's coercion rules, including the cases that must be rejected.
func TestScalarInputCoercion(t *testing.T) {
	e := newScalarInputExecutor(t)

	for _, tc := range []struct {
		name    string
		query   string
		vars    string
		want    string // expected data payload, when the input is valid
		wantErr string // substring of the expected error, when it is not
	}{
		// Int
		{name: "int plain", query: `query($v: Int!){ i(v: $v) }`, vars: `{"v":42}`, want: `{"i":"42"}`},
		{name: "int negative", query: `query($v: Int!){ i(v: $v) }`, vars: `{"v":-7}`, want: `{"i":"-7"}`},
		{name: "int from whole float", query: `query($v: Int!){ i(v: $v) }`, vars: `{"v":1.0}`, want: `{"i":"1"}`},
		{name: "int rejects fraction", query: `query($v: Int!){ i(v: $v) }`, vars: `{"v":1.5}`, wantErr: "non-integer"},
		{name: "int rejects 32-bit overflow", query: `query($v: Int!){ i(v: $v) }`, vars: `{"v":2147483648}`, wantErr: "32-bit"},
		{name: "int rejects string", query: `query($v: Int!){ i(v: $v) }`, vars: `{"v":"x"}`, wantErr: "Int"},

		// Float
		{name: "float plain", query: `query($v: Float!){ f(v: $v) }`, vars: `{"v":1.5}`, want: `{"f":"1.5"}`},
		{name: "float from int", query: `query($v: Float!){ f(v: $v) }`, vars: `{"v":2}`, want: `{"f":"2"}`},
		{name: "float rejects string", query: `query($v: Float!){ f(v: $v) }`, vars: `{"v":"x"}`, wantErr: "Float"},

		// ID bound to a string
		{name: "id string", query: `query($v: ID!){ ids(v: $v) }`, vars: `{"v":"abc"}`, want: `{"ids":"abc"}`},
		{name: "id from integer", query: `query($v: ID!){ ids(v: $v) }`, vars: `{"v":42}`, want: `{"ids":"42"}`},
		{name: "id rejects fraction", query: `query($v: ID!){ ids(v: $v) }`, vars: `{"v":1.5}`, wantErr: "ID"},

		// ID bound to an integer
		{name: "id int from numeric string", query: `query($v: ID!){ idi(v: $v) }`, vars: `{"v":"42"}`, want: `{"idi":"42"}`},
		{name: "id int from number", query: `query($v: ID!){ idi(v: $v) }`, vars: `{"v":7}`, want: `{"idi":"7"}`},
		{name: "id int rejects words", query: `query($v: ID!){ idi(v: $v) }`, vars: `{"v":"abc"}`, wantErr: "ID"},

		// String and Boolean
		{name: "string plain", query: `query($v: String!){ s(v: $v) }`, vars: `{"v":"hi"}`, want: `{"s":"hi"}`},
		{name: "string rejects number", query: `query($v: String!){ s(v: $v) }`, vars: `{"v":1}`, wantErr: "String"},
		{name: "bool plain", query: `query($v: Boolean!){ b(v: $v) }`, vars: `{"v":true}`, want: `{"b":"true"}`},
		{name: "bool rejects string", query: `query($v: Boolean!){ b(v: $v) }`, vars: `{"v":"true"}`, wantErr: "Boolean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, e, tc.query, tc.vars)
			if tc.wantErr != "" {
				if len(resp.Errors) == 0 {
					t.Fatalf("want an error containing %q, got data %s", tc.wantErr, resp.Data)
				}
				if !strings.Contains(resp.Errors[0].Message, tc.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", resp.Errors[0].Message, tc.wantErr)
				}
				return
			}
			expectData(t, resp, tc.want)
		})
	}
}

// TestScalarInputCoercionAsLiterals runs the same rules as inline literals,
// which reach the decoders by a different route than variables.
func TestScalarInputCoercionAsLiterals(t *testing.T) {
	e := newScalarInputExecutor(t)

	expectData(t, run(t, e, `{ i(v: 42) }`, ""), `{"i":"42"}`)
	expectData(t, run(t, e, `{ f(v: 1.5) }`, ""), `{"f":"1.5"}`)
	expectData(t, run(t, e, `{ ids(v: "abc") }`, ""), `{"ids":"abc"}`)
	expectData(t, run(t, e, `{ idi(v: 7) }`, ""), `{"idi":"7"}`)
	expectData(t, run(t, e, `{ b(v: false) }`, ""), `{"b":"false"}`)

	resp := run(t, e, `{ i(v: 2147483648) }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a literal outside 32-bit Int must be rejected")
	}
}

// TestScalarDecodersAcceptNativeGoNumbers covers the decoders' defensive
// branches. The engine never reaches them -- decodeVariables and asJSON
// normalise every number to json.Number first -- but they define what the
// built-in decoders accept when called with a Go value directly.
func TestScalarDecodersAcceptNativeGoNumbers(t *testing.T) {
	for _, raw := range []any{int64(5), int(5), int32(5), float64(5)} {
		got, err := rawInt64(raw, "Int")
		if err != nil || got != 5 {
			t.Errorf("rawInt64(%T %v) = (%d, %v), want (5, nil)", raw, raw, got, err)
		}
	}
	for _, raw := range []any{float64(1.5), math.Inf(1), math.NaN()} {
		if _, err := rawInt64(raw, "Int"); err == nil {
			t.Errorf("rawInt64(%v) = nil error, want a rejection", raw)
		}
	}

	for _, raw := range []any{float64(2.5), float32(2.5)} {
		got, err := decodeFloat64(raw)
		if err != nil || got != 2.5 {
			t.Errorf("decodeFloat64(%T) = (%v, %v), want (2.5, nil)", raw, got, err)
		}
	}
	for _, raw := range []any{int64(3), int(3), int32(3)} {
		got, err := decodeFloat64(raw)
		if err != nil || got != 3 {
			t.Errorf("decodeFloat64(%T) = (%v, %v), want (3, nil)", raw, got, err)
		}
	}
	if _, err := decodeFloat64(true); err == nil {
		t.Error("decodeFloat64(bool) = nil error, want a rejection")
	}

	for _, raw := range []any{int64(9), int(9), int32(9)} {
		got, err := decodeIDString(raw)
		if err != nil || got != "9" {
			t.Errorf("decodeIDString(%T) = (%q, %v), want (\"9\", nil)", raw, got, err)
		}
	}
}

// TestDescribeRawRendersEveryKind covers the helper that builds the input
// half of every coercion error message.
func TestDescribeRawRendersEveryKind(t *testing.T) {
	for _, tc := range []struct {
		raw  any
		want string
	}{
		{nil, "null"},
		{"x", `"x"`},
		{json.Number("1.5"), "1.5"},
		{true, "true"},
	} {
		if got := describeRaw(tc.raw); got != tc.want {
			t.Errorf("describeRaw(%#v) = %s, want %s", tc.raw, got, tc.want)
		}
	}
}
