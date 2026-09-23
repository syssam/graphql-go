package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// astJSON says it "evaluates an AST value the way a transport would have
// decoded JSON: integers and floats become json.Number so custom scalars see
// the same representation for literals and variables". That is the contract
// between this engine and every custom scalar an author writes, and nothing
// checked it.
//
// The built-in decoders accept json.Number, float64 and int64 alike, so a
// divergence is invisible to them and to every test that goes through them. A
// custom scalar receives `raw any` verbatim: if a literal arrived as int64
// where a variable arrives as json.Number, an unmarshal func written against
// one form silently fails for the other, and which form a value takes would
// depend on whether the client inlined it or parameterised it.

type probeScalar struct{ text string }

// seen records the Go type and value each unmarshal call received.
type seen struct{ typ, val string }

func probeSchema(t *testing.T, rec *[]seen) *Schema {
	t.Helper()
	type args struct{ V *probeScalar }
	s, err := NewSchema(SDL(`
		scalar Probe
		type Query { take(v: Probe): String! }
	`),
		Scalar[probeScalar]("Probe",
			func(w *Writer, v probeScalar) error { w.String(v.text); return nil },
			func(raw any) (probeScalar, error) {
				*rec = append(*rec, seen{fmt.Sprintf("%T", raw), fmt.Sprintf("%v", raw)})
				return probeScalar{text: fmt.Sprintf("%v", raw)}, nil
			},
		),
		Args[args](InputField("v", func(a *args, v *probeScalar) { a.V = v })),
		Query(ResolveArgs("take", func(_ context.Context, _ Root, a args) (string, error) {
			if a.V == nil {
				return "<nil>", nil
			}
			return a.V.text, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}

func TestACustomScalarSeesLiteralsAndVariablesAlike(t *testing.T) {
	for _, c := range []struct {
		name    string
		literal string // as written inline in the query
		varJSON string // the same value as a JSON variable
	}{
		{"int", `5`, `5`},
		{"negative int", `-5`, `-5`},
		{"float", `1.5`, `1.5`},
		{"exponent", `1e3`, `1e3`},
		{"negative exponent", `1e-7`, `1e-7`},
		{"trailing zero float", `1.0`, `1.0`},
		// Past float64 precision: routing a literal through float64 turned
		// this into 9.007199254740992e+15 while the variable kept its text,
		// so a Decimal scalar decoded a different number depending on whether
		// the client inlined it.
		{"float past float64 precision", `9007199254740993.0`, `9007199254740993.0`},
		// Wider than int64: gqlparser strconv.ParseInt-s this and panics, and
		// validation cannot help because a custom scalar accepts anything.
		{"integer wider than int64", `123456789012345678901234567890`, `123456789012345678901234567890`},
		{"string", `"abc"`, `"abc"`},
		{"boolean", `true`, `true`},
		{"null", `null`, `null`},
		{"list of ints", `[1, 2, 3]`, `[1,2,3]`},
		{"list of mixed", `[1, "a", true]`, `[1,"a",true]`},
		{"object", `{a: 1, b: "x"}`, `{"a":1,"b":"x"}`},
		{"nested", `{a: [1, {b: 2.5}]}`, `{"a":[1,{"b":2.5}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var litSeen, varSeen []seen

			e := NewExecutor(probeSchema(t, &litSeen))
			lit := run(t, e, `{ take(v: `+c.literal+`) }`, "")
			if len(lit.Errors) > 0 {
				t.Fatalf("literal %s: %s", c.literal, errorsJSON(lit.Errors))
			}

			e = NewExecutor(probeSchema(t, &varSeen))
			vr := run(t, e, `query($v: Probe){ take(v: $v) }`, `{"v":`+c.varJSON+`}`)
			if len(vr.Errors) > 0 {
				t.Fatalf("variable %s: %s", c.varJSON, errorsJSON(vr.Errors))
			}

			// A null never reaches the scalar from either form.
			if c.literal == "null" {
				if len(litSeen) != 0 || len(varSeen) != 0 {
					t.Fatalf("null reached the scalar: literal %v, variable %v", litSeen, varSeen)
				}
				return
			}
			// Both forms must reach the scalar, and every call in a request must
			// see the same thing -- the variable form decodes twice (see
			// TestACustomScalarUnmarshalRunsTwiceForAVariable), so compare all
			// of them rather than assuming one.
			if len(litSeen) == 0 || len(varSeen) == 0 {
				t.Fatalf("unmarshal calls: literal %d, variable %d, want at least 1 each",
					len(litSeen), len(varSeen))
			}
			for _, got := range append(append([]seen{}, litSeen...), varSeen...) {
				if got.typ != litSeen[0].typ {
					t.Errorf("a custom scalar saw %s for the literal and %s for the variable; "+
						"an unmarshal func written against one form fails for the other, and "+
						"which form a value takes would depend on whether the client inlined it",
						litSeen[0].typ, got.typ)
				}
				if got.val != litSeen[0].val {
					t.Errorf("value differs across forms: %q vs %q", litSeen[0].val, got.val)
				}
			}
			if got, want := string(lit.Data), string(vr.Data); got != want {
				t.Errorf("response differs: literal %s, variable %s", got, want)
			}
		})
	}
}

// And the representation is specifically json.Number, not float64: a scalar
// carrying an id or a decimal needs the text to stay exact, which is the whole
// reason the doc names json.Number rather than "some number type".
func TestANumericScalarArgumentIsAJSONNumber(t *testing.T) {
	for _, c := range []struct{ name, literal, varJSON string }{
		{"int", `5`, `5`},
		{"big int past float64 precision", `9007199254740993`, `9007199254740993`},
		{"float", `1.5`, `1.5`},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, form := range []struct {
				label, query, vars string
			}{
				{"literal", `{ take(v: ` + c.literal + `) }`, ""},
				{"variable", `query($v: Probe){ take(v: $v) }`, `{"v":` + c.varJSON + `}`},
			} {
				var rec []seen
				e := NewExecutor(probeSchema(t, &rec))
				if resp := run(t, e, form.query, form.vars); len(resp.Errors) > 0 {
					t.Fatalf("%s: %s", form.label, errorsJSON(resp.Errors))
				}
				if len(rec) == 0 {
					t.Fatalf("%s: the scalar was never called", form.label)
				}
				if rec[0].typ != fmt.Sprintf("%T", json.Number("")) {
					t.Errorf("%s: scalar saw %s, want json.Number -- a float64 would lose "+
						"the exact text an id or a decimal needs", form.label, rec[0].typ)
				}
				if rec[0].val != c.literal {
					t.Errorf("%s: scalar saw %q, want the text as written (%q)",
						form.label, rec[0].val, c.literal)
				}
			}
		})
	}
}

// A custom scalar's unmarshal runs once for a literal and *twice* for a
// variable, and that is not written down anywhere. The second call is
// registerLeaf's leaf validator, which is the decoder itself: coerceVariables
// decodes the value to find out whether it is acceptable, discards the result
// so a variable error is reported before execution starts, and the argument
// decode then does the same work again.
//
// It matters for anyone whose unmarshal is not free -- parsing a decimal, a
// UUID, a timestamp, decoding base64 -- or not pure, because a counter or a
// log line inside it doubles for exactly the clients that parameterise their
// queries, which is the clients following best practice. This pins the count
// so that changing it is a decision rather than a surprise, in either
// direction.
func TestACustomScalarUnmarshalRunsTwiceForAVariable(t *testing.T) {
	var lit, vr []seen

	e := NewExecutor(probeSchema(t, &lit))
	if resp := run(t, e, `{ take(v: 5) }`, ""); len(resp.Errors) > 0 {
		t.Fatalf("literal: %s", errorsJSON(resp.Errors))
	}
	if len(lit) != 1 {
		t.Errorf("literal form called unmarshal %d times, want 1", len(lit))
	}

	e = NewExecutor(probeSchema(t, &vr))
	if resp := run(t, e, `query($v: Probe){ take(v: $v) }`, `{"v":5}`); len(resp.Errors) > 0 {
		t.Fatalf("variable: %s", errorsJSON(resp.Errors))
	}
	if len(vr) != 2 {
		t.Errorf("variable form called unmarshal %d times, want 2 (validate then decode); "+
			"if this is now 1 the validator stopped decoding and the saving is real, "+
			"but Scalar's godoc says the count and has to change with it", len(vr))
	}
}
