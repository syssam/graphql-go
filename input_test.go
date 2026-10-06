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

// Each of these is a binding NewSchema refuses outright. Without the refusal
// the second Input replaced the first's decoder under shapes already handed
// out, a non-struct reached reflect's field walk and panicked there, and a
// list bound to a single value was decoded by whichever decoder the Go type
// happened to select.
func TestMalformedInputAndArgsBindingsAreBuildErrors(t *testing.T) {
	type in struct{ A *int }
	type other struct{ A *int }
	type args struct{ In *in }
	type flat struct {
		A []int
		B *int
	}
	const sdl = `input In { a: Int } input Lists { a: Int b: [Int] } type Query { q(in: In): Int }`
	query := Query(FieldArgs("q", func(Root, args) *int { return nil }))
	for _, tc := range []struct {
		name string
		opts []SchemaOption
		want []string
	}{
		{"an input object bound twice", []SchemaOption{Input[in]("In"), Input[other]("In")},
			[]string{`Input "In": already bound to graphql.in`}},
		{"an argument struct registered twice", []SchemaOption{Input[in]("In"), Args[args](), Args[args]()},
			[]string{"Args[graphql.args]: registered more than once"}},
		{"an input object bound to a non-struct", []SchemaOption{Input[string]("In")},
			[]string{`Input "In": Go type string must be a struct`}},
		{"arguments bound to a non-struct", []SchemaOption{Input[in]("In"), Args[int]()},
			[]string{"Args[int]: Go type must be a struct"}},
		{"a list where the schema has one value, and one value where it has a list", []SchemaOption{Input[in]("In"), Input[flat]("Lists")},
			[]string{"Lists.a: Go type []int has 1 list level(s) but Int has 0", "Lists.b: Go type *int has 0 list level(s) but [Int] has 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(SDL(sdl), append(tc.opts, query)...)
			if err == nil {
				t.Fatal("the schema built")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not say %q:\n%v", want, err)
				}
			}
		})
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
	// An argument struct nobody registered is derived, as Args[args]() would
	// derive it; see args_derive_test.go.
	if err != nil {
		t.Fatalf("an unregistered argument struct that fits the field should be derived, got %v", err)
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

// A default is a value like any other and has to be one its position accepts.
// gqlparser checks only that the SDL parses, so `n: Int = "abc"` built, and
// every request that left the argument out failed with an error about a value
// the client never sent -- on a schema the author believed was valid.
func TestADefaultThePositionRefusesIsABuildError(t *testing.T) {
	type inner struct{ Need int }
	type opts struct {
		N      *int
		C      *string
		Nested *inner
		List   []int
		Fine   *int
	}
	type good struct {
		A     *int
		Inner *inner
	}
	type nArgs struct {
		N    *int
		Fine *int
	}
	type dArgs struct{ Level *int }
	err := Validate(SDL(`
		directive @d(level: Int = "high") on FIELD_DEFINITION
		enum Color { RED GREEN }
		input Inner { need: Int! }
		input Opts { n: Int = "abc" c: Color = PURPLE nested: Inner = {} list: [Int!] = [1, "x"] fine: Int = 3 }
		input Good { a: Int = 1 inner: Inner = {need: 1} }
		type Query {
			byNumber(n: Int = 1.5, fine: Int = 1): Int
			byColor(c: Color! = null): Int
			guarded: Int @d
			sound(in: Good = {a: 5}, c: Color = RED): Int
		}`),
		Enum("Color", map[string]string{"RED": "RED", "GREEN": "GREEN"}),
		Input[inner]("Inner"),
		Input[opts]("Opts"),
		Input[good]("Good"),
		DirectiveArgs("d", func(next FieldFunc, _ dArgs) FieldFunc { return next }),
		Query(
			FieldArgs("byNumber", func(Root, nArgs) *int { return nil }),
			FieldArgs("byColor", func(Root, struct{ C string }) *int { return nil }),
			Field("guarded", func(Root) *int { return nil }),
			FieldArgs("sound", func(Root, struct {
				In *good
				C  *string
			}) *int {
				return nil
			}),
		),
	)
	if err == nil {
		t.Fatal("the schema built; each of these defaults fails the first request that relies on it")
	}
	for _, want := range []string{
		`input field Opts.n: its default "abc" is not a value it accepts: Int cannot represent non-integer value: "abc"`,
		`input field Opts.c: its default PURPLE is not a value it accepts: value "PURPLE" does not exist in Color enum`,
		`input field Opts.nested: its default {} is not a value it accepts: field "need" of required type Int! was not provided`,
		`input field Opts.list: its default [1,"x"] is not a value it accepts: [1]: Int cannot represent non-integer value: "x"`,
		`field Query.byNumber: argument "n": its default 1.5 is not a value it accepts: Int cannot represent non-integer value: 1.5`,
		`field Query.byColor: argument "c": its default null is not a value it accepts: null value for non-null type`,
		`field @d: argument "level": its default "high" is not a value it accepts: Int cannot represent non-integer value: "high"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%v", want, err)
		}
	}
	for _, sound := range []string{"Opts.fine", "Good", "Query.sound"} {
		if strings.Contains(err.Error(), sound) {
			t.Errorf("a default that does coerce (%s) was reported:\n%v", sound, err)
		}
	}
}

// A default that leaves out a field whose own default is the same kind of
// value never finishes: `input A { a: A = {} }` decodes {} by applying a's
// default, which is {}. The specification forbids the cycle, gqlparser does not
// look, and decoding it is unbounded recursion -- a stack overflow, which no
// recover catches, on the first request to send an A.
func TestADefaultThatLeadsBackToItsOwnFieldIsABuildError(t *testing.T) {
	type a struct{ A *a }
	for _, tc := range []struct {
		name, sdl string
		want      string
	}{
		{"itself", `input A { a: A = {} }`, `input field A.a: its default {} leads back to it through A.a`},
		{"through another input", `input A { a: B = {} } input B { b: A = {} }`, `input field A.a: its default {} leads back to it through A.a, B.b`},
		{"through a list", `input A { a: [A] = [{}] }`, `input field A.a: its default [{}] leads back to it through A.a`},
		{"below what the default supplies", `input A { a: A = {a: {}} }`, `input field A.a: its default {a:{}} leads back to it through A.a`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(SDL(tc.sdl+` type Query { f(in: A = {}): Int }`),
				Query(Field("f", func(Root) *int { return nil })))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error saying %q, got %v", tc.want, err)
			}
		})
	}

	// Bound, the cycle is still only reported: checking a default decodes it,
	// and decoding this one is the recursion.
	err := Validate(SDL(`input A { a: A = {} } type Query { f(in: A = {}): Int }`),
		Input[a]("A"),
		Query(FieldArgs("f", func(Root, struct{ In *a }) *int { return nil })))
	if err == nil || !strings.Contains(err.Error(), "input field A.a: its default {} leads back to it through A.a") {
		t.Fatalf("got %v", err)
	}

	// A default that ends is not a cycle, however the type refers to itself.
	for _, sdl := range []string{
		`input A { a: A }`,
		`input A { a: A = null }`,
		`input A { a: A = {a: null} }`,
		`input A { a: A = {a: {a: null}} }`,
	} {
		err := Validate(SDL(sdl+` type Query { f(in: A = {}): Int }`),
			Input[a]("A"),
			Query(FieldArgs("f", func(Root, struct{ In *a }) *int { return nil })))
		if err != nil {
			t.Errorf("%s: %v", sdl, err)
		}
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

		// An initialism is a word, wherever it sits and whatever follows it:
		// the predicates an ORM generates put a suffix straight after one.
		"IDs":            "ids",
		"UserIDs":        "userIds",
		"IDIn":           "idIn",
		"IDNEQ":          "idNEQ",
		"OwnerIDNEQ":     "ownerIdNEQ",
		"VariantIDIsNil": "variantIdIsNil",
		"URLs":           "urls",
		"URLPath":        "urlPath",
		"ImageURLID":     "imageUrlId",
		"CreatedAt":      "createdAt",
		"XCoord":         "xCoord",
		"IOSVersion":     "iOSVersion",

		// UUID and GUID end in ID without being a word followed by one, and
		// were split there: uUId, gUId, userUUId.
		"UUID":       "uuid",
		"GUID":       "guid",
		"UUIDs":      "uuids",
		"UUIDIn":     "uuidIn",
		"UserUUID":   "userUuid",
		"ParentGUID": "parentGuid",
		"UUIDURL":    "uuidUrl",
	}
	for in, want := range cases {
		if got := exportedToGraphQL(in); got != want {
			t.Errorf("exportedToGraphQL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Teaching the derivation UUID and GUID must change nothing else: a struct
// whose fields bind by their Go names today has to bind to the same names
// after. Every name over the letters the rules look at, to six of them, is
// derived as it was unless it spells one of the two.
func TestExportedToGraphQLChangesOnlyUUIDAndGUID(t *testing.T) {
	before := func(name string) string {
		name = strings.ReplaceAll(name, "ID", "Id")
		name = strings.ReplaceAll(name, "URL", "Url")
		return strings.ToLower(name[:1]) + name[1:]
	}
	const letters = "IDURLGXas"
	checked := 0
	var walk func(name []byte)
	walk = func(name []byte) {
		if len(name) > 0 {
			n := string(name)
			got := exportedToGraphQL(n)
			if strings.Contains(n, "UUID") || strings.Contains(n, "GUID") {
				if strings.Contains(got, "UUId") || strings.Contains(got, "GUId") {
					t.Fatalf("exportedToGraphQL(%q) = %q still splits the initialism", n, got)
				}
			} else if want := before(n); got != want {
				t.Fatalf("exportedToGraphQL(%q) = %q, and it was %q", n, got, want)
			}
			checked++
		}
		if len(name) == 6 {
			return
		}
		for i := range len(letters) {
			walk(append(name, letters[i]))
		}
	}
	walk(make([]byte, 0, 6))
	if checked < 500_000 {
		t.Fatalf("checked %d names", checked)
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
	fields := autoInputFields(reflect.TypeFor[wrapped](), false, nil)
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

// Every Go shape an input object can be decoded into, derived and
// hand-written: the decoders behind them are type-erased, and a hand-written
// InputField reaches them through a typed adapter of its own.
func TestInputObjectShapes(t *testing.T) {
	const sdl = `
		input Nested { tag: String! }
		input Shapes { vals: [Nested!], ptrs: [Nested], one: Nested!, opt: Nested }
		input Hand { vals: [Nested!] }
		type Query { q(s: Shapes!, h: Hand!): Int }
	`
	type shapes struct {
		Vals []nestedIn
		Ptrs []*nestedIn
		One  nestedIn
		Opt  *nestedIn
	}
	type hand struct{ Vals []nestedIn }
	s, err := NewSchema(SDL(sdl),
		Input[nestedIn]("Nested"),
		Input[shapes]("Shapes"),
		Input[hand]("Hand", InputField("vals", func(h *hand, v []nestedIn) { h.Vals = v })),
		Query(ResolveArgs("q", func(context.Context, Root, struct {
			S shapes
			H hand
		}) (*int, error) {
			return nil, nil
		})),
		Args[struct {
			S shapes
			H hand
		}](),
	)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(name string, raw map[string]any) (any, error) {
		t.Helper()
		return s.reg.inputsByName[name].decode(raw)
	}
	tag := func(s string) map[string]any { return map[string]any{"tag": s} }

	v, err := decode("Shapes", map[string]any{
		"vals": []any{tag("a"), tag("b")},
		"ptrs": []any{tag("c"), nil},
		"one":  tag("d"),
		"opt":  nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(*shapes)
	if len(got.Vals) != 2 || got.Vals[0].Tag != "a" || got.Vals[1].Tag != "b" {
		t.Errorf("[Nested!] into []T: %+v", got.Vals)
	}
	if len(got.Ptrs) != 2 || got.Ptrs[0].Tag != "c" || got.Ptrs[1] != nil {
		t.Errorf("[Nested] into []*T keeps a null element as nil: %+v", got.Ptrs)
	}
	if got.One.Tag != "d" || got.Opt != nil {
		t.Errorf("Nested! into T, null into *T: %+v", got)
	}

	// A single value coerces to a one-element list.
	v, err = decode("Shapes", map[string]any{"vals": tag("x"), "one": tag("d")})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(*shapes).Vals; len(got) != 1 || got[0].Tag != "x" {
		t.Errorf("list coercion into []T: %+v", got)
	}

	// A null element of [Nested!] names its index.
	if _, err := decode("Shapes", map[string]any{"vals": []any{tag("a"), nil}, "one": tag("d")}); err == nil || !strings.Contains(err.Error(), "[1]") {
		t.Errorf("null element of [Nested!]: %v", err)
	}

	v, err = decode("Hand", map[string]any{"vals": []any{tag("h1"), tag("h2")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(*hand).Vals; len(got) != 2 || got[1].Tag != "h2" {
		t.Errorf("InputField of []T: %+v", got)
	}
}
