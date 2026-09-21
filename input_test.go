package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type nestedIn struct {
	Tag string
}

type testIn struct {
	A int
	B *string
	C []int
	D Omittable[*string]
	E *nestedIn
	F []*nestedIn
	G *string
}

const inputSDL = `
input Nested { tag: String! }
input In {
  a: Int!
  b: String
  c: [Int!]
  d: String
  e: Nested
  f: [Nested!]
  g: String = "dflt"
}
type Query { q(in: In!): Int }
`

type qArgs struct{ In testIn }

func inputOptions() []SchemaOption {
	return []SchemaOption{
		Input[nestedIn]("Nested", InputField("tag", func(n *nestedIn, v string) { n.Tag = v })),
		Input[testIn]("In",
			InputField("a", func(t *testIn, v int) { t.A = v }),
			InputField("b", func(t *testIn, v *string) { t.B = v }),
			InputField("c", func(t *testIn, v []int) { t.C = v }),
			OmittableField("d", func(t *testIn, v Omittable[*string]) { t.D = v }),
			InputField("e", func(t *testIn, v *nestedIn) { t.E = v }),
			InputField("f", func(t *testIn, v []*nestedIn) { t.F = v }),
			InputField("g", func(t *testIn, v *string) { t.G = v }),
		),
		Args[qArgs](InputField("in", func(a *qArgs, v testIn) { a.In = v })),
		Object[Root]("Query", ResolveArgs("q", func(context.Context, Root, qArgs) (*int, error) { return nil, nil })),
	}
}

func decodeIn(t *testing.T, s *Schema, rawJSON string) (*testIn, error) {
	t.Helper()
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(rawJSON))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	v, err := s.reg.inputsByName["In"].decode(raw)
	if err != nil {
		return nil, err
	}
	return v.(*testIn), nil
}

func TestInputDecoding(t *testing.T) {
	s, err := NewSchema(SDL(inputSDL), inputOptions()...)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("required missing", func(t *testing.T) {
		_, err := decodeIn(t, s, `{}`)
		if err == nil || !strings.Contains(err.Error(), `"a"`) {
			t.Fatalf("expected missing a error, got %v", err)
		}
	})

	t.Run("defaults and absence", func(t *testing.T) {
		v, err := decodeIn(t, s, `{"a": 1}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.A != 1 || v.B != nil || v.C != nil || v.D.IsSet() || v.E != nil || v.F != nil {
			t.Fatalf("unexpected: %+v", v)
		}
		if v.G == nil || *v.G != "dflt" {
			t.Fatalf("default not applied: %v", v.G)
		}
	})

	t.Run("explicit null vs value", func(t *testing.T) {
		v, err := decodeIn(t, s, `{"a": 1, "b": null, "d": null, "g": null}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.B != nil {
			t.Fatal("explicit null b must be nil")
		}
		if !v.D.IsSet() || v.D.Value() != nil {
			t.Fatalf("explicit null d must be set with nil value: %+v", v.D)
		}
		if v.G != nil {
			t.Fatal("explicit null must override default")
		}

		v, err = decodeIn(t, s, `{"a": 1, "b": "x", "d": "y"}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.B == nil || *v.B != "x" || !v.D.IsSet() || *v.D.Value() != "y" {
			t.Fatalf("unexpected: %+v", v)
		}
	})

	t.Run("list coercion", func(t *testing.T) {
		v, err := decodeIn(t, s, `{"a": 1, "c": 5}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.C) != 1 || v.C[0] != 5 {
			t.Fatalf("single value must coerce to list: %v", v.C)
		}
		v, err = decodeIn(t, s, `{"a": 1, "c": [1, 2]}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.C) != 2 || v.C[1] != 2 {
			t.Fatalf("list: %v", v.C)
		}
		_, err = decodeIn(t, s, `{"a": 1, "c": [1, null]}`)
		if err == nil {
			t.Fatal("null element in [Int!] must fail")
		}
	})

	t.Run("nested objects", func(t *testing.T) {
		v, err := decodeIn(t, s, `{"a": 1, "e": {"tag": "t"}, "f": [{"tag": "x"}, {"tag": "y"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.E == nil || v.E.Tag != "t" || len(v.F) != 2 || v.F[1].Tag != "y" {
			t.Fatalf("unexpected: %+v", v)
		}
		_, err = decodeIn(t, s, `{"a": 1, "e": {"tag": null}}`)
		if err == nil || !strings.Contains(err.Error(), `"tag"`) {
			t.Fatalf("nested non-null violation should fail with field name, got %v", err)
		}
		_, err = decodeIn(t, s, `{"a": 1, "e": 3}`)
		if err == nil {
			t.Fatal("non-object for input object must fail")
		}
	})

	t.Run("type errors carry field", func(t *testing.T) {
		_, err := decodeIn(t, s, `{"a": "one"}`)
		if err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "Int") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestInputShapeValidation(t *testing.T) {
	sdl := `input In { a: Int, b: [Int] } type Query { q(in: In!): Int }`
	type in struct {
		A int
		B []int
	}
	type args struct{ In in }
	err := Validate(SDL(sdl),
		Input[in]("In",
			InputField("a", func(t *in, v int) { t.A = v }),
			InputField("b", func(t *in, v []int) { t.B = v }),
		),
		Args[args](InputField("in", func(a *args, v in) { a.In = v })),
		Object[Root]("Query", ResolveArgs("q", func(context.Context, Root, args) (*int, error) { return nil, nil })),
	)
	if err == nil {
		t.Fatal("expected shape errors")
	}
	msg := err.Error()
	if !strings.Contains(msg, "In.a") || !strings.Contains(msg, "cannot represent null") {
		t.Errorf("nullable Int bound to int should fail: %s", msg)
	}
	if !strings.Contains(msg, "In.b") {
		t.Errorf("nullable list element bound to int should fail: %s", msg)
	}
}

func TestInputCoverageAndUnknownField(t *testing.T) {
	sdl := `input In { a: Int! b: Int! } type Query { q(in: In!): Int }`
	type in struct{ A, B, C int }
	type args struct{ In in }
	err := Validate(SDL(sdl),
		Input[in]("In",
			InputField("a", func(t *in, v int) { t.A = v }),
			InputField("c", func(t *in, v int) { t.C = v }),
		),
		Args[args](InputField("in", func(a *args, v in) { a.In = v })),
		Object[Root]("Query", ResolveArgs("q", func(context.Context, Root, args) (*int, error) { return nil, nil })),
	)
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	if !strings.Contains(msg, "In.c is not defined") {
		t.Errorf("unknown field should be reported: %s", msg)
	}
	if !strings.Contains(msg, "In.b has no binding") {
		t.Errorf("unbound field should be reported: %s", msg)
	}
}

func TestArgsValidation(t *testing.T) {
	sdl := `type Query { q(x: Int!, y: String): Int, noArgs: Int }`
	type args struct {
		X int
		Y *string
	}
	type wrongArgs struct{ Z int }
	resolver := func(context.Context, Root, args) (*int, error) { return nil, nil }

	err := Validate(SDL(sdl),
		Object[Root]("Query",
			ResolveArgs("q", resolver),
			Field("noArgs", func(Root) *int { return nil }),
		),
	)
	if err == nil || !strings.Contains(err.Error(), "no Args[") {
		t.Fatalf("missing Args registration should fail, got %v", err)
	}

	err = Validate(SDL(sdl),
		Args[args](InputField("x", func(a *args, v int) { a.X = v })),
		Object[Root]("Query",
			ResolveArgs("q", resolver),
			Field("noArgs", func(Root) *int { return nil }),
		),
	)
	if err == nil || !strings.Contains(err.Error(), `argument "y" has no binding`) {
		t.Fatalf("unbound argument should fail, got %v", err)
	}

	err = Validate(SDL(sdl),
		Args[wrongArgs](InputField("z", func(a *wrongArgs, v int) { a.Z = v })),
		Object[Root]("Query",
			ResolveArgs("q", func(context.Context, Root, wrongArgs) (*int, error) { return nil, nil }),
			Field("noArgs", func(Root) *int { return nil }),
		),
	)
	if err == nil || !strings.Contains(err.Error(), `argument "z" is not defined`) {
		t.Fatalf("unknown argument should fail, got %v", err)
	}

	err = Validate(SDL(sdl),
		Args[args](
			InputField("x", func(a *args, v int) { a.X = v }),
			InputField("y", func(a *args, v *string) { a.Y = v }),
		),
		Object[Root]("Query",
			ResolveArgs("q", resolver),
			ResolveArgs("noArgs", resolver),
		),
	)
	if err == nil || !strings.Contains(err.Error(), "the field has none") {
		t.Fatalf("args binding on argument-less field should fail, got %v", err)
	}
}

func TestExportedToGraphQL(t *testing.T) {
	cases := map[string]string{
		"ID":        "id",
		"Name":      "name",
		"AuthorID":  "authorId",
		"URL":       "url",
		"HomeURL":   "homeUrl",
		"firstName": "firstName",
	}
	for in, want := range cases {
		if got := exportedToGraphQL(in); got != want {
			t.Errorf("exportedToGraphQL(%q) = %q, want %q", in, got, want)
		}
	}
}

type autoArgs struct {
	AuthorID ID
	First    *int
	Hidden   string `graphql:"-"`
	Renamed  string `graphql:"term"`
}

func TestAutoArgsAndInput(t *testing.T) {
	const sdl = `
		input Filter { authorId: ID, first: Int = 2 }
		type Query { search(authorId: ID!, first: Int = 2, term: String!): String! }
	`
	type filter struct {
		AuthorID *ID
		First    *int
	}
	s, err := NewSchema(SDL(sdl),
		Input[filter]("Filter"),
		Args[autoArgs](),
		Query(FieldArgs("search", func(_ Root, a autoArgs) string {
			return string(a.AuthorID) + ":" + a.Renamed
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	resp := run(t, e, `{ search(authorId: "1", term: "go") }`, "")
	expectData(t, resp, `{"search":"1:go"}`)

	dec := s.reg.inputsByName["Filter"]
	v, err := dec.decode(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if *v.(*filter).First != 2 {
		t.Fatalf("precomputed default first = %v", v.(*filter).First)
	}
}

func TestAutoOmittable(t *testing.T) {
	const sdl = `input Patch { title: String } type Query { p(in: Patch!): String! }`
	type patch struct {
		Title Omittable[*string]
	}
	type args struct{ In patch }
	s, err := NewSchema(SDL(sdl),
		Input[patch]("Patch"),
		Args[args](),
		Query(ResolveArgs("p", func(_ context.Context, _ Root, a args) (string, error) {
			if !a.In.Title.IsSet() {
				return "absent", nil
			}
			if a.In.Title.Value() == nil {
				return "null", nil
			}
			return *a.In.Title.Value(), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)
	expectData(t, run(t, e, `{ p(in: {}) }`, ""), `{"p":"absent"}`)
	expectData(t, run(t, e, `{ p(in: {title: null}) }`, ""), `{"p":"null"}`)
	expectData(t, run(t, e, `{ p(in: {title: "Hi"}) }`, ""), `{"p":"Hi"}`)
}

func TestAutoFieldCount(t *testing.T) {
	type embed struct{ Tag string }
	type wrapped struct {
		embed
		Name string
		// Unexported on purpose: autoInputFields must not report it, which
		// is what the count below asserts. Never read, by design.
		//lint:ignore U1000 the point of this field is that nothing uses it
		skip string
	}
	fields := autoInputFields(reflect.TypeFor[wrapped]())
	if len(fields) != 2 {
		t.Fatalf("auto fields = %d, want 2 (embed.Tag + Name)", len(fields))
	}
}

// --- literal argument coercion ------------------------------------------

type coerceColor string

type coerceIn struct {
	S      Omittable[*string]
	Block  *string
	I      *int
	F      *float64
	B      *bool
	C      *coerceColor
	Nil    Omittable[*string]
	List   []int
	Nested *coerceIn
}

type coerceArgs struct{ In coerceIn }

func renderCoerce(in *coerceIn) string {
	if in == nil {
		return "<nil>"
	}
	var b strings.Builder
	writeOpt := func(name string, o Omittable[*string]) {
		v, ok := o.ValueOK()
		switch {
		case !ok:
			fmt.Fprintf(&b, "%s=absent ", name)
		case v == nil:
			fmt.Fprintf(&b, "%s=null ", name)
		default:
			fmt.Fprintf(&b, "%s=%q ", name, *v)
		}
	}
	writeOpt("s", in.S)
	writeOpt("nil", in.Nil)
	if in.Block != nil {
		fmt.Fprintf(&b, "block=%q ", *in.Block)
	}
	if in.I != nil {
		fmt.Fprintf(&b, "i=%d ", *in.I)
	}
	if in.F != nil {
		fmt.Fprintf(&b, "f=%v ", *in.F)
	}
	if in.B != nil {
		fmt.Fprintf(&b, "b=%v ", *in.B)
	}
	if in.C != nil {
		fmt.Fprintf(&b, "c=%s ", string(*in.C))
	}
	if in.List != nil {
		fmt.Fprintf(&b, "list=%v ", in.List)
	}
	if in.Nested != nil {
		fmt.Fprintf(&b, "nested=(%s) ", strings.TrimSpace(renderCoerce(in.Nested)))
	}
	return strings.TrimSpace(b.String())
}

func newCoerceExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
		enum Color { RED GREEN }
		input CoerceIn {
			s: String
			block: String
			i: Int
			f: Float
			b: Boolean
			c: Color
			nil: String
			list: [Int!]
			nested: CoerceIn
		}
		type Query { echo(in: CoerceIn!): String! }
	`),
		Enum[coerceColor]("Color", map[coerceColor]string{"RED": "RED", "GREEN": "GREEN"}),
		Input[coerceIn]("CoerceIn"),
		Args[coerceArgs](),
		Query(ResolveArgs("echo", func(_ context.Context, _ Root, a coerceArgs) (string, error) {
			return renderCoerce(&a.In), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

// TestLiteralArgumentKinds walks every ast.Value kind astJSON decodes: ints,
// floats, strings, block strings, enums, booleans, null, lists and nested
// objects, as literals rather than variables.
func TestLiteralArgumentKinds(t *testing.T) {
	e := newCoerceExecutor(t)
	resp := run(t, e, `{ echo(in: {
		s: "x"
		block: """line"""
		i: 7
		f: 1.5
		b: true
		c: GREEN
		nil: null
		list: [1, 2, 3]
		nested: { s: "inner", i: 9 }
	}) }`, "")
	expectData(t, resp,
		`{"echo":"s=\"x\" nil=null block=\"line\" i=7 f=1.5 b=true c=GREEN list=[1 2 3] nested=(s=\"inner\" nil=absent i=9)"}`)
}

// TestLiteralObjectOmitsAbsentVariable pins the specification rule that makes
// Omittable meaningful: an input object field whose value is a variable that
// was not supplied is left out entirely, rather than coerced to null.
func TestLiteralObjectOmitsAbsentVariable(t *testing.T) {
	e := newCoerceExecutor(t)

	resp := run(t, e, `query ($v: String) { echo(in: { s: $v }) }`, "")
	expectData(t, resp, `{"echo":"s=absent nil=absent"}`)

	resp = run(t, e, `query ($v: String) { echo(in: { s: $v }) }`, `{"v":null}`)
	expectData(t, resp, `{"echo":"s=null nil=absent"}`)

	resp = run(t, e, `query ($v: String) { echo(in: { s: $v }) }`, `{"v":"given"}`)
	expectData(t, resp, `{"echo":"s=\"given\" nil=absent"}`)
}

// TestLiteralListOfObjects covers list values whose elements are objects,
// which recurse through astJSON twice.
func TestLiteralListOfObjects(t *testing.T) {
	e := newCoerceExecutor(t)
	resp := run(t, e, `{ echo(in: { nested: { nested: { i: 1 } } }) }`, "")
	expectData(t, resp, `{"echo":"s=absent nil=absent nested=(s=absent nil=absent nested=(s=absent nil=absent i=1))"}`)
}

// TestVariableDefaultKinds covers astJSON, which decodes default values
// rather than field arguments: a variable default carrying every literal
// kind, none of them supplied by the caller.
func TestVariableDefaultKinds(t *testing.T) {
	e := newCoerceExecutor(t)
	resp := run(t, e, `query ($in: CoerceIn = {
		s: "def"
		block: """blk"""
		i: 1
		f: 2.5
		b: false
		c: RED
		nil: null
		list: [4, 5]
		nested: { i: 2 }
	}) { echo(in: $in) }`, "")
	expectData(t, resp,
		`{"echo":"s=\"def\" nil=null block=\"blk\" i=1 f=2.5 b=false c=RED list=[4 5] nested=(s=absent nil=absent i=2)"}`)
}
