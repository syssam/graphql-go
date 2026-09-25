package graphql

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Validation and decoding visit an input value's own keys, and a Go map has
// no order. These pin what that must not change: which error is reported,
// the order hand-written setters run in, and what an absent field means.

type fpInput struct {
	A *int `json:"a"`
	B *int `json:"b"`
	C *int `json:"c"`
	D *int `json:"d"`
	E *int `json:"e"`
}

type fpStrict struct {
	A *fpCode `json:"a"`
	B *fpCode `json:"b"`
	C *fpCode `json:"c"`
}

type fpCode string

type fpDefaults struct {
	Limit int     `json:"limit"`
	Tag   *string `json:"tag"`
	Need  int     `json:"need"`
}

type fpOrdered struct{ seen []string }

type (
	fpInArgs struct {
		In *fpInput `graphql:"in"`
	}
	fpStrictArgs struct {
		S *fpStrict `graphql:"s"`
	}
	fpDefArgs struct {
		Def *fpDefaults `graphql:"def"`
	}
	fpOrderedArgs struct {
		O *fpOrdered `graphql:"o"`
	}
)

func fpExec(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
scalar Code
input In { a: Int b: Int c: Int d: Int e: Int }
input Strict { a: Code b: Code c: Code }
input Defaults { limit: Int! = 10 tag: String = "t" need: Int! }
input Ordered { w: Int x: Int y: Int z: Int }
type Query {
  in(in: In): String!
  strict(s: Strict): String!
  def(def: Defaults!): String!
  ordered(o: Ordered): String!
}`),
		Scalar("Code", func(w *Writer, c fpCode) error { w.String(string(c)); return nil },
			func(v any) (fpCode, error) {
				if s, _ := v.(string); s != "bad" {
					return fpCode(s), nil
				}
				return "", errors.New("bad code")
			}),
		Input[fpInput]("In"),
		Input[fpStrict]("Strict"),
		Input[fpDefaults]("Defaults"),
		Input[fpOrdered]("Ordered",
			InputField("w", func(o *fpOrdered, _ *int) { o.seen = append(o.seen, "w") }),
			InputField("x", func(o *fpOrdered, _ *int) { o.seen = append(o.seen, "x") }),
			InputField("y", func(o *fpOrdered, _ *int) { o.seen = append(o.seen, "y") }),
			InputField("z", func(o *fpOrdered, _ *int) { o.seen = append(o.seen, "z") }),
		),
		Args[fpInArgs](), Args[fpStrictArgs](), Args[fpDefArgs](), Args[fpOrderedArgs](),
		Query(
			ResolveArgs("in", func(context.Context, Root, fpInArgs) (string, error) { return "ok", nil }),
			ResolveArgs("strict", func(context.Context, Root, fpStrictArgs) (string, error) { return "ok", nil }),
			ResolveArgs("def", func(_ context.Context, _ Root, a fpDefArgs) (string, error) {
				tag := "<nil>"
				if a.Def.Tag != nil {
					tag = *a.Def.Tag
				}
				return tag + " " + itoa(int64(a.Def.Limit)) + " " + itoa(int64(a.Def.Need)), nil
			}),
			ResolveArgs("ordered", func(_ context.Context, _ Root, a fpOrderedArgs) (string, error) {
				return strings.Join(a.O.seen, ""), nil
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

// Five fields wrong at once: the error names the first in declared order,
// on every run, whether the value arrived as a variable or a literal.
func TestInputErrorsDoNotDependOnMapOrder(t *testing.T) {
	e := fpExec(t)
	for _, c := range []struct{ name, query, vars, want string }{
		{"variable", `query($v: In) { in(in: $v) }`, `{"v":{"e":"x","d":"x","c":"x","b":"x","a":"x"}}`, "$v.a"},
		{"literal", `{ strict(s: {c: "bad", b: "bad", a: "bad"}) }`, "", `field "a"`},
	} {
		first := ""
		for i := range 40 {
			resp := run(t, e, c.query, c.vars)
			if len(resp.Errors) != 1 {
				t.Fatalf("%s: %s", c.name, errorsJSON(resp.Errors))
			}
			msg := resp.Errors[0].Message
			if i == 0 {
				first = msg
				if !strings.Contains(msg, c.want) {
					t.Fatalf("%s: %q does not name %s", c.name, msg, c.want)
				}
			} else if msg != first {
				t.Fatalf("%s: run %d reported %q, run 0 %q", c.name, i, msg, first)
			}
		}
	}
}

// A hand-written InputField is arbitrary code, and these four append to one
// slice: they run in declared order, never in the value's map order.
func TestHandWrittenInputFieldsRunInDeclaredOrder(t *testing.T) {
	e := fpExec(t)
	for range 40 {
		expectData(t, run(t, e, `query($o: Ordered) { ordered(o: $o) }`, `{"o":{"z":1,"y":1,"x":1,"w":1}}`), `{"ordered":"wxyz"}`)
	}
}

// Visiting only the keys a value has must still give an absent field its
// SDL default, and still refuse an absent required one.
func TestAbsentInputFieldsKeepTheirMeaning(t *testing.T) {
	e := fpExec(t)
	expectData(t, run(t, e, `query($d: Defaults!) { def(def: $d) }`, `{"d":{"need":3}}`), `{"def":"t 10 3"}`)
	expectData(t, run(t, e, `{ def(def: {need: 4, tag: null}) }`, ""), `{"def":"<nil> 10 4"}`)
	resp := run(t, e, `query($d: Defaults!) { def(def: $d) }`, `{"d":{"limit":1}}`)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, `"need" of required type "Int!" was not provided at $d`) {
		t.Fatalf("a missing required field: %s", errorsJSON(resp.Errors))
	}
}
