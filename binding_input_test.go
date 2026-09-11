package graphql

import (
	"context"
	"encoding/json"
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
