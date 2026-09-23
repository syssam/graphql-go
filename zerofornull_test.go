package graphql

import (
	"context"
	"strings"
	"testing"
)

type zfnFilter struct {
	IsNil bool   `json:"isNil"`
	Name  string `json:"name"`
}

type zfnArgs struct{ F *zfnFilter }

const zfnSDL = `
input Filter { isNil: Boolean name: String }
type Query { q(f: Filter): String! }
`

// A nullable input position normally needs a Go type that can be null, because
// a bool cannot tell absent from false. An ORM-generated filter means the
// opposite on purpose: entgql emits `IsNil bool` under `isNil: Boolean` and
// reads false as "no predicate", so absent, null and false are one value. That
// is 11 035 fields on one real schema, in SDL its author does not hand-write.
// ZeroForNull says so per input type rather than weakening the rule globally.
func TestZeroForNullIsRefusedByDefault(t *testing.T) {
	_, err := NewSchema(SDL(zfnSDL), Input[zfnFilter]("Filter"))
	if err == nil {
		t.Fatal("a bool at a nullable position was accepted without ZeroForNull")
	}
	if !strings.Contains(err.Error(), "cannot represent null") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestZeroForNullAcceptsAValueAtANullablePosition(t *testing.T) {
	s, err := NewSchema(SDL(zfnSDL),
		Input[zfnFilter]("Filter", ZeroForNull()),
		Query(ResolveArgs("q", func(_ context.Context, _ Root, a zfnArgs) (string, error) {
			if a.F == nil {
				return "absent", nil
			}
			return map[bool]string{true: "T", false: "F"}[a.F.IsNil] + ":" + a.F.Name, nil
		})),
		Args[zfnArgs](),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	for _, c := range []struct{ query, want string }{
		// The three spellings that must agree: a value, an explicit null, and
		// the field left out entirely.
		{`{ q(f: {isNil: true, name: "a"}) }`, `{"q":"T:a"}`},
		{`{ q(f: {isNil: false, name: "a"}) }`, `{"q":"F:a"}`},
		{`{ q(f: {isNil: null, name: "a"}) }`, `{"q":"F:a"}`},
		{`{ q(f: {name: "a"}) }`, `{"q":"F:a"}`},
		{`{ q(f: {isNil: true, name: null}) }`, `{"q":"T:"}`},
	} {
		resp := e.Execute(context.Background(), &Request{Query: c.query})
		if len(resp.Errors) > 0 {
			t.Errorf("%s: errors: %v", c.query, resp.Errors)
		} else if got := string(resp.Data); got != c.want {
			t.Errorf("%s: got %s want %s", c.query, got, c.want)
		}
		resp.Release()
	}
}

// The option is per type: it must not leak to an input that did not ask.
func TestZeroForNullDoesNotLeakToOtherInputs(t *testing.T) {
	const sdl = `
input Lax { isNil: Boolean }
input Strict { isNil: Boolean }
type Query { q(a: Lax, b: Strict): String! }
`
	type lax struct {
		IsNil bool `json:"isNil"`
	}
	type strict struct {
		IsNil bool `json:"isNil"`
	}
	_, err := NewSchema(SDL(sdl), Input[lax]("Lax", ZeroForNull()), Input[strict]("Strict"))
	if err == nil {
		t.Fatal("Strict was accepted; ZeroForNull leaked across bindings")
	}
	if strings.Contains(err.Error(), "Lax") {
		t.Errorf("Lax was refused although it opted in: %v", err)
	}
}

type zfnOmit struct {
	Flag Omittable[bool] `json:"flag"`
}

type zfnOmitArgs struct{ F *zfnOmit }

// ZeroForNull and Omittable look like opposites -- one says absent, null and
// false are the same value, the other exists to keep them apart -- and a field
// can carry both. What they mean together is the only reading that loses
// nothing: null is answered with the zero value like any other ZeroForNull
// field, and IsSet still separates that from a field the client left out.
//
// This is the branch of Omittable.assign that nothing else reaches
// (assign(nil, true)), and the setter that ZeroForNull added for it.
func TestZeroForNullKeepsOmittableAbleToSeeAbsence(t *testing.T) {
	s, err := NewSchema(SDL(`
		input OmitFilter { flag: Boolean }
		type Query { q(f: OmitFilter): String! }
	`),
		Input[zfnOmit]("OmitFilter", ZeroForNull()),
		Args[zfnOmitArgs](),
		Query(ResolveArgs("q", func(_ context.Context, _ Root, a zfnOmitArgs) (string, error) {
			if a.F == nil {
				return "no input", nil
			}
			if !a.F.Flag.IsSet() {
				return "omitted", nil
			}
			return map[bool]string{true: "set:true", false: "set:false"}[a.F.Flag.Value()], nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	for _, c := range []struct{ query, want string }{
		{`{ q(f: {flag: true}) }`, `{"q":"set:true"}`},
		{`{ q(f: {flag: false}) }`, `{"q":"set:false"}`},
		// An explicit null is the zero value, and still distinguishable from
		// a field that was never sent -- which is the whole point of pairing
		// the two.
		{`{ q(f: {flag: null}) }`, `{"q":"set:false"}`},
		{`{ q(f: {}) }`, `{"q":"omitted"}`},
	} {
		expectData(t, run(t, e, c.query, ""), c.want)
	}
}
