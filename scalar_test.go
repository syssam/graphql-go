package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"

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

type money int64

func moneySchema(t *testing.T, marshal func(*Writer, money) error, unmarshal func(any) (money, error)) *Executor {
	t.Helper()
	type qArgs struct{ V money }
	type nArgs struct{ N *money }
	type lArgs struct{ Ls []money }
	s, err := NewSchema(SDL(`
		scalar Money
		type Query {
			p: Money!
			pn: Money
			ps: [Money!]!
			pp: [Money]!
			pl: [[Money!]!]!
			q(v: Money!): String!
			n(n: Money): String!
			l(ls: [Money!]!): String!
		}`),
		Scalar("Money", marshal, unmarshal),
		Args[qArgs](), Args[nArgs](), Args[lArgs](),
		Query(
			Field("p", func(Root) money { return 1 }),
			Field("pn", func(Root) *money { return nil }),
			Field("ps", func(Root) []money { return []money{1, 2} }),
			Field("pp", func(Root) []*money { m := money(3); return []*money{&m, nil} }),
			Field("pl", func(Root) [][]money { return [][]money{{1}, {2, 3}} }),
			ResolveArgs("q", func(_ context.Context, _ Root, a qArgs) (string, error) { return fmt.Sprint(a.V), nil }),
			ResolveArgs("n", func(_ context.Context, _ Root, a nArgs) (string, error) {
				if a.N == nil {
					return "nil", nil
				}
				return fmt.Sprint(*a.N), nil
			}),
			ResolveArgs("l", func(_ context.Context, _ Root, a lArgs) (string, error) { return fmt.Sprint(a.Ls), nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

func goodMoney(w *Writer, m money) error { w.String("$" + itoa(int64(m))); return nil }

// calls counts unmarshal runs so the documented "twice for a variable, once for
// a literal" cost is pinned rather than only described.
func TestCustomScalarEndToEnd(t *testing.T) {
	var calls int
	e := moneySchema(t, goodMoney, func(raw any) (money, error) {
		calls++
		n, err := rawInt64(raw, "Money")
		if err == nil && n < 0 {
			return 0, fmt.Errorf("Money cannot be negative: %d", n)
		}
		return money(n), err
	})

	expectData(t, run(t, e, `{ p pn ps pp pl }`, ""),
		`{"p":"$1","pn":null,"ps":["$1","$2"],"pp":["$3",null],"pl":[["$1"],["$2","$3"]]}`)

	for _, tc := range []struct{ name, query, vars, want, wantErr string }{
		{"literal", `{ q(v: 5) }`, "", `{"q":"5"}`, ""},
		{"variable", `query($v: Money!){ q(v: $v) }`, `{"v":6}`, `{"q":"6"}`, ""},
		{"nullable literal null", `{ n(n: null) }`, "", `{"n":"nil"}`, ""},
		{"nullable omitted", `{ n }`, "", `{"n":"nil"}`, ""},
		{"nullable value", `query($v: Money){ n(n: $v) }`, `{"v":7}`, `{"n":"7"}`, ""},
		{"nullable variable null", `query($v: Money){ n(n: $v) }`, `{"v":null}`, `{"n":"nil"}`, ""},
		{"list", `query($v: [Money!]!){ l(ls: $v) }`, `{"v":[1,2]}`, `{"l":"[1 2]"}`, ""},
		{"non-null rejects null", `query($v: Money!){ q(v: $v) }`, `{"v":null}`, "", "Money"},
		{"unmarshal error, variable", `query($v: Money!){ q(v: $v) }`, `{"v":-1}`, "", "cannot be negative"},
		{"unmarshal error, literal", `{ q(v: -1) }`, "", "", "cannot be negative"},
		{"unmarshal error in list", `query($v: [Money!]!){ l(ls: $v) }`, `{"v":[1,-2]}`, "", "cannot be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, e, tc.query, tc.vars)
			if tc.wantErr != "" {
				if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, tc.wantErr) {
					t.Fatalf("want error containing %q, got %v / %s", tc.wantErr, resp.Errors, resp.Data)
				}
				return
			}
			expectData(t, resp, tc.want)
		})
	}

	// A literal is decoded when its plan compiles, so this text must be new.
	calls = 0
	run(t, e, `{ q(v: 9) }`, "")
	if calls != 1 {
		t.Errorf("a literal ran unmarshal %d times, want 1", calls)
	}
	calls = 0
	run(t, e, `query($v: Money!){ q(v: $v) }`, `{"v":5}`)
	if calls != 2 {
		t.Errorf("a variable ran unmarshal %d times, want 2", calls)
	}
}

// TestCustomScalarMarshalMustWriteOneValue fails with the output check removed:
// the first two cases then produce invalid JSON and no error.
func TestCustomScalarMarshalMustWriteOneValue(t *testing.T) {
	dec := func(any) (money, error) { return 0, nil }
	for _, tc := range []struct {
		name    string
		marshal func(*Writer, money) error
		want    string
	}{
		{"nothing", func(*Writer, money) error { return nil }, "exactly one JSON value"},
		{"two values", func(w *Writer, _ money) error { w.String("a"); w.String("b"); return nil }, "exactly one JSON value"},
		{"invalid raw", func(w *Writer, _ money) error { w.Raw([]byte(`{`)); return nil }, "exactly one JSON value"},
		{"partial then error", func(w *Writer, _ money) error { w.String("a"); return fmt.Errorf("boom") }, "scalar Money: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := moneySchema(t, tc.marshal, dec)
			resp := run(t, e, `{ p }`, "")
			if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, tc.want) {
				t.Fatalf("errors = %v, want one containing %q (data %s)", resp.Errors, tc.want, resp.Data)
			}
			if string(resp.Data) != "null" {
				t.Fatalf("data = %s, want null: the bad value must not leak into the response", resp.Data)
			}
			if !json.Valid(resp.Data) {
				t.Fatalf("response data is not valid JSON: %s", resp.Data)
			}
		})
	}

	// A sibling field is not poisoned: a nullable position takes the error alone.
	e := moneySchema(t, func(*Writer, money) error { return nil }, dec)
	resp := run(t, e, `{ pn }`, "")
	expectData(t, resp, `{"pn":null}`)
}

// The output check runs for every custom scalar value written, so it has to
// cost no allocation: it reads the bytes where the marshal left them.
func TestScalarOutputCheckDoesNotAllocate(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	e := moneySchema(t, func(w *Writer, m money) error { w.Int64(int64(m)); return nil }, func(any) (money, error) { return 0, nil })
	write := e.schema.reg.leafWriters[typeKey{"Money", reflect.TypeFor[money]()}].(func(*jsonw.Writer, money, *ast.Type) error)
	typ := ast.NamedType("Money", nil)
	var w jsonw.Writer
	if n := testing.AllocsPerRun(200, func() {
		w.Reset()
		w.BeginArray()
		_ = write(&w, 1, typ)
		_ = write(&w, 2, typ)
		w.EndArray()
	}); n != 0 {
		t.Fatalf("writing two checked scalar values allocated %v times", n)
	}
	if got := string(w.Bytes()); got != "[1,2]" {
		t.Fatalf("wrote %s", got)
	}
}

// Float is a float64 on the wire, and a field bound to float32 holds less.
// The conversion does not fail, it saturates: 1e300 reached the resolver as
// +Inf, a value no client can send and no Float can be written as.
func TestFloat32RefusesAValueOutsideItsRange(t *testing.T) {
	r := newTestRegistry()
	dec := decoder[float32](t, r, "Float")
	nn := parseType("Float!")
	for _, raw := range []any{json.Number("1e300"), json.Number("-1e300"), json.Number("3.5e38"), float64(1e39), int64(math.MaxInt64)} {
		if raw == any(int64(math.MaxInt64)) {
			// Large, and well inside float32's range: it only loses digits.
			if got, err := dec(raw, nn); err != nil || got != float32(math.MaxInt64) {
				t.Errorf("Float(%v) = %v, %v", raw, got, err)
			}
			continue
		}
		if got, err := dec(raw, nn); err == nil || !strings.Contains(err.Error(), "32-bit") {
			t.Errorf("Float(%v) as float32 = %v, %v; want a range error", raw, got, err)
		}
	}
	for raw, want := range map[json.Number]float32{"3.4e38": 3.4e38, "-3.4e38": -3.4e38, "1.5": 1.5, "1e-60": 0} {
		if got, err := dec(raw, nn); err != nil || got != want {
			t.Errorf("Float(%s) as float32 = %v, %v; want %v", raw, got, err, want)
		}
	}

	e := float32EchoExecutor(t)
	resp := run(t, e, `{ f(x: 1e300) }`, "")
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "Float cannot represent non 32-bit floating-point value: 1e300") {
		t.Fatalf("data %s, errors %s", resp.Data, errorsJSON(resp.Errors))
	}
	expectData(t, run(t, e, `{ f(x: 2.5) }`, ""), `{"f":2.5}`)

	// And on the way out, a float32 that is not a number is refused as a
	// float64 is.
	w := jsonw.New()
	for _, v := range []float32{float32(math.Inf(1)), float32(math.NaN())} {
		if err := leafWriter[float32](t, r, "Float")(w, v, nn); err == nil || !strings.Contains(err.Error(), "non numeric") {
			t.Errorf("writing float32 %v: %v", v, err)
		}
	}
}

func float32EchoExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`type Query { f(x: Float!): Float! }`),
		Query(FieldArgs("f", func(_ Root, a struct{ X float32 }) float32 { return a.X })))
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

// oneJSONValue stands in for json.Valid on what a scalar wrote, so it has to
// answer as json.Valid does: on the shapes it recognises itself, on the ones
// just outside them, and on bytes it was never meant for.
func TestOneJSONValueAgreesWithJSONValid(t *testing.T) {
	cases := []string{
		``, `"`, `""`, `"a"`, `"a`, `a"`, `"a"b"`, `"a\"b"`, `"a\\"`, `"a\"`, "\"a\nb\"", "\"\x01\"", "\"\xff\"", `"é"`,
		`0`, `-0`, `00`, `-`, `1`, `-1`, `12`, `01`, `1.5`, `1e3`, `1.`, `-01`, `+1`, `1 2`, `1,2`,
		`true`, `false`, `null`, `tru`, `nulll`, `True`, `t`, `n`,
		`{}`, `[]`, `{"a":1}`, `[1,2]`, `{`, `[`, `"a","b"`, `"a","b":"c"`, ` "a"`, `"a" `,
		`123e4567-e89b-12d3-a456-426614174000`,
	}
	for _, c := range cases {
		if got, want := oneJSONValue([]byte(c)), json.Valid([]byte(c)); got != want {
			t.Errorf("oneJSONValue(%q) = %t, json.Valid says %t", c, got, want)
		}
	}
	// Every string of up to four bytes over an alphabet that reaches each
	// branch and each way out of it.
	const alphabet = "\"\\-0127tfnue. ,[{\x01"
	var walk func(prefix []byte, left int)
	walk = func(prefix []byte, left int) {
		if got, want := oneJSONValue(prefix), json.Valid(prefix); got != want {
			t.Fatalf("oneJSONValue(%q) = %t, json.Valid says %t", prefix, got, want)
		}
		if left == 0 {
			return
		}
		for i := range len(alphabet) {
			walk(append(prefix[:len(prefix):len(prefix)], alphabet[i]), left-1)
		}
	}
	walk(nil, 4)

	// Strings long enough to be read a word at a time, with each byte that
	// matters at each position, among bytes that only look like one in part.
	for length := 2; length <= 40; length++ {
		for at := 0; at < length; at++ {
			for _, special := range []byte{0x00, 0x1f, 0x20, '"', '\\', 0x7f, 0x80, 0xa0, 0xa2, 0xdc, 0xff} {
				b := bytes.Repeat([]byte{0x21}, length)
				b[0], b[length-1] = '"', '"'
				b[at] = special
				if got, want := oneJSONValue(b), json.Valid(b); got != want {
					t.Fatalf("oneJSONValue(%q) = %t, json.Valid says %t", b, got, want)
				}
			}
		}
	}
}
