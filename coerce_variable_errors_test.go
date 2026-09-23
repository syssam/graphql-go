package graphql

import (
	"context"
	"strings"
	"testing"
)

// validateInput produces the message a client sees when it sends a bad
// variable, which is the most common client-facing failure any GraphQL API
// has. Eight of its branches had no test, so eight distinct messages could
// have been wrong, missing or pointing at the wrong path and nothing would
// have said so.
//
// It also claims to "mirror the checks the typed decoders perform so that
// variable errors are reported before execution starts". That is a second,
// stronger property: whatever it accepts the decoder must accept, and whatever
// it rejects the decoder would have rejected too. A validator more permissive
// than its decoder turns a clean variable error into a worse one at execution;
// a stricter one refuses input the engine can handle.

const varErrSDL = `
enum Color { RED GREEN }
input Point { x: Int! y: Int label: String }
type Query {
  scalar(v: Int!): String!
  list(v: [Int]): String!
  nested(v: [[Int]]): String!
  color(v: Color): String!
  point(v: Point): String!
  points(v: [Point!]): String!
}
`

type veArgsInt struct{ V int }
type veArgsList struct{ V []*int }
type veArgsNested struct{ V [][]*int }
type veArgsColor struct{ V *string }
type vePoint struct {
	X     int
	Y     *int
	Label *string
}
type veArgsPoint struct{ V *vePoint }
type veArgsPoints struct{ V []*vePoint }

func varErrExec(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(varErrSDL),
		Enum[string]("Color", map[string]string{"RED": "RED", "GREEN": "GREEN"}),
		Input[vePoint]("Point",
			InputField("x", func(p *vePoint, v int) { p.X = v }),
			InputField("y", func(p *vePoint, v *int) { p.Y = v }),
			InputField("label", func(p *vePoint, v *string) { p.Label = v }),
		),
		Args[veArgsInt](InputField("v", func(a *veArgsInt, v int) { a.V = v })),
		Args[veArgsList](InputField("v", func(a *veArgsList, v []*int) { a.V = v })),
		Args[veArgsNested](InputField("v", func(a *veArgsNested, v [][]*int) { a.V = v })),
		Args[veArgsColor](InputField("v", func(a *veArgsColor, v *string) { a.V = v })),
		Args[veArgsPoint](InputField("v", func(a *veArgsPoint, v *vePoint) { a.V = v })),
		Args[veArgsPoints](InputField("v", func(a *veArgsPoints, v []*vePoint) { a.V = v })),
		Query(
			ResolveArgs("scalar", func(context.Context, Root, veArgsInt) (string, error) { return "ok", nil }),
			ResolveArgs("list", func(context.Context, Root, veArgsList) (string, error) { return "ok", nil }),
			ResolveArgs("nested", func(context.Context, Root, veArgsNested) (string, error) { return "ok", nil }),
			ResolveArgs("color", func(context.Context, Root, veArgsColor) (string, error) { return "ok", nil }),
			ResolveArgs("point", func(context.Context, Root, veArgsPoint) (string, error) { return "ok", nil }),
			ResolveArgs("points", func(context.Context, Root, veArgsPoints) (string, error) { return "ok", nil }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

func TestVariableValidationMessages(t *testing.T) {
	e := varErrExec(t)
	for _, c := range []struct {
		name, query, vars, want string
	}{
		{
			"null for a non-null variable",
			`query($v: Int!){ scalar(v: $v) }`, `{"v":null}`,
			`of non-null type "Int!" must not be null`,
		},
		{
			"null inside a non-null input field",
			`query($v: Point){ point(v: $v) }`, `{"v":{"x":null}}`,
			`Expected non-nullable type "Int!" not to be null at $v.x`,
		},
		{
			"an element of a list fails, and the index is named",
			`query($v: [Int]){ list(v: $v) }`, `{"v":[1,"no",3]}`,
			`at $v[1]`,
		},
		{
			"enum given a non-string",
			`query($v: Color){ color(v: $v) }`, `{"v":7}`,
			`Enum "Color" cannot represent non-string value`,
		},
		{
			"enum value not in the enum",
			`query($v: Color){ color(v: $v) }`, `{"v":"BLUE"}`,
			`Value "BLUE" does not exist in "Color" enum`,
		},
		{
			"input object given a non-object",
			`query($v: Point){ point(v: $v) }`, `{"v":7}`,
			`Expected type "Point" to be an object`,
		},
		{
			"unknown field on an input object",
			`query($v: Point){ point(v: $v) }`, `{"v":{"x":1,"nope":2}}`,
			`Field "nope" is not defined by type "Point"`,
		},
		{
			"required input field missing",
			`query($v: Point){ point(v: $v) }`, `{"v":{"y":1}}`,
			`Field "x" of required type "Int!" was not provided`,
		},
		{
			"a nested element names its full path",
			`query($v: [Point!]){ points(v: $v) }`, `{"v":[{"x":1},{"y":2}]}`,
			`at $v[1]`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := run(t, e, c.query, c.vars)
			if len(resp.Errors) == 0 {
				t.Fatalf("accepted %s; data = %s", c.vars, resp.Data)
			}
			// The message itself, not errorsJSON: the JSON form escapes the
			// quotes the GraphQL specification puts around type names.
			msg := resp.Errors[0].Message
			if !strings.Contains(msg, c.want) {
				t.Errorf("message does not contain %q:\n  %s", c.want, msg)
			}
			body := errorsJSON(resp.Errors)
			if !strings.Contains(body, "BAD_USER_INPUT") {
				t.Errorf("a bad variable is the client's fault and must carry "+
					"BAD_USER_INPUT:\n%s", body)
			}
			if resp.Data != nil {
				t.Errorf("a refused variable still produced data: %s", resp.Data)
			}
		})
	}
}

// The spec coerces a single value at a list position into a one-element list,
// and validateInput implements that by validating the value against the
// element type. The decoder has to agree, or a variable the validator waved
// through fails later with a worse message -- which is the exact divergence
// the "mirrors the checks the typed decoders perform" claim forbids.
func TestASingleValueCoercesToAOneElementList(t *testing.T) {
	e := varErrExec(t)
	for _, c := range []struct{ name, query, vars string }{
		{"flat list", `query($v: [Int]){ list(v: $v) }`, `{"v":5}`},
		{"nested list", `query($v: [[Int]]){ nested(v: $v) }`, `{"v":5}`},
		{"list of input objects", `query($v: [Point!]){ points(v: $v) }`, `{"v":{"x":1}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := run(t, e, c.query, c.vars)
			if len(resp.Errors) > 0 {
				t.Fatalf("a single value at a list position was refused, so the "+
					"validator and the decoder disagree about list coercion: %s",
					errorsJSON(resp.Errors))
			}
		})
	}
}

// And the mirror in the other direction: a single value that is wrong for the
// element type must still be refused, rather than being waved through because
// it was not a list.
func TestASingleValueAtAListPositionIsStillChecked(t *testing.T) {
	e := varErrExec(t)
	resp := run(t, e, `query($v: [Int]){ list(v: $v) }`, `{"v":"no"}`)
	if len(resp.Errors) == 0 {
		t.Fatalf("a string was accepted for [Int]; data = %s", resp.Data)
	}
	if body := errorsJSON(resp.Errors); !strings.Contains(body, "Int") {
		t.Errorf("message does not name the element type:\n%s", body)
	}
}
