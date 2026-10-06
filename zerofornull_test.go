package graphql

import (
	"context"
	"encoding/json"
	"fmt"
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

type zfnLists struct {
	Tags   []string    `json:"tags"`
	Grid   [][]int     `json:"grid"`
	Kids   []zfnFilter `json:"kids"`
	Ptrs   []*string   `json:"ptrs"`
	Strict []string    `json:"strict"`
	Rows   [][]int     `json:"rows"`
}

// A null inside a list is the same promise one level down. The build already
// accepted []string under [String] for a ZeroForNull binding, and then the
// element decoder refused the null the schema allows there -- a request the
// SDL calls valid failed with "null value for non-null type". A null element
// is the zero value of whatever the Go slice holds, which for a pointer is
// still nil, and a non-null element type refuses null as it always did.
func TestZeroForNullReachesListElements(t *testing.T) {
	s, err := NewSchema(SDL(`
		input Filter { isNil: Boolean name: String }
		input Lists { tags: [String] grid: [[Int]] kids: [Filter] ptrs: [String] strict: [String!] rows: [[Int]!] }
		type Query { l(in: Lists!): String! }`),
		Input[zfnFilter]("Filter", ZeroForNull()),
		Input[zfnLists]("Lists", ZeroForNull()),
		Query(FieldArgs("l", func(_ Root, a struct{ In zfnLists }) string {
			ptrs := make([]string, len(a.In.Ptrs))
			for i, p := range a.In.Ptrs {
				ptrs[i] = "nil"
				if p != nil {
					ptrs[i] = *p
				}
			}
			return fmt.Sprintf("%q %v %v %v", a.In.Tags, a.In.Grid, a.In.Kids, ptrs)
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	for _, c := range []struct{ in, want string }{
		{`{tags: ["a", null]}`, `["a" ""] [] [] []`},
		{`{tags: null}`, `[] [] [] []`},
		{`{grid: [[1, null], null, 3]}`, `[] [[1 0] [] [3]] [] []`},
		{`{kids: [{isNil: true}, null]}`, `[] [] [{true } {false }] []`},
		{`{ptrs: ["a", null]}`, `[] [] [] [a nil]`},
	} {
		want, _ := json.Marshal(map[string]string{"l": c.want})
		expectData(t, run(t, e, `{ l(in: `+c.in+`) }`, ""), string(want))

		// The same value as a variable takes the other route in: coerced
		// against the SDL first, then decoded.
		vars := strings.NewReplacer("tags:", `"tags":`, "grid:", `"grid":`, "kids:", `"kids":`, "isNil:", `"isNil":`, "ptrs:", `"ptrs":`).Replace(c.in)
		expectData(t, run(t, e, `query($in: Lists!) { l(in: $in) }`, `{"in":`+vars+`}`), string(want))
	}

	resp := run(t, e, `query($in: Lists!) { l(in: $in) }`, `{"in":{"strict":["a",null]}}`)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "String!") {
		t.Fatalf("a null element of [String!] must still be refused, got data %s errors %s", resp.Data, errorsJSON(resp.Errors))
	}

	// Validation refuses a null at a non-null level before any decoder sees
	// it; the decoder is asked directly because it is the last thing that
	// would. In [[Int]!] the rows cannot be null and their numbers can.
	dec := s.reg.inputsByName["Lists"]
	got, err := dec.decode(map[string]any{"rows": []any{[]any{json.Number("1"), nil}}})
	if err != nil || fmt.Sprint(got.(*zfnLists).Rows) != "[[1 0]]" {
		t.Fatalf("rows = %v, %v", got, err)
	}
	if _, err := dec.decode(map[string]any{"rows": []any{[]any{json.Number("1")}, nil}}); err == nil || !strings.Contains(err.Error(), "[1]: null value for non-null type") {
		t.Fatalf("a null row of [[Int]!] was decoded: %v", err)
	}
}
