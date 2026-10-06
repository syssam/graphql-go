package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vektah/gqlparser/v2/ast"
	"reflect"
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

// A variable's default is a value like any other and has to coerce to the
// variable's type. One that did not was handed on unchecked: the operation
// ran, sibling resolvers with it, and the bad value failed one field -- where
// the same value written as a literal is refused before anything runs.
func TestAnUncoercibleVariableDefaultIsARequestError(t *testing.T) {
	type args struct{ L *int }
	ran := 0
	s, err := NewSchema(SDL(`input In { n: Int } type Query { f(l: Int): Int g(in: In): Int side: Int }`),
		Args[args](),
		Input[struct{ N *int }]("In"),
		Args[struct{ In *struct{ N *int } }](),
		Query(
			FieldArgs("f", func(_ Root, a args) *int { return a.L }),
			FieldArgs("g", func(Root, struct{ In *struct{ N *int } }) *int { return nil }),
			Field("side", func(Root) *int { ran++; return nil }),
		))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	for _, q := range []string{
		`query($l: Int = 99999999999) { side f(l: $l) }`,
		`query($in: In = {n: 99999999999}) { side g(in: $in) }`,
	} {
		ran = 0
		resp := run(t, e, q, "")
		if resp.Data != nil || len(resp.Errors) != 1 {
			t.Fatalf("%s: data=%s errors=%s, want a request error and no data", q, resp.Data, errorsJSON(resp.Errors))
		}
		if ran != 0 {
			t.Fatalf("%s: a resolver ran for an operation whose variables do not coerce", q)
		}
		if msg := resp.Errors[0].Message; !strings.Contains(msg, "default") {
			t.Errorf("%s: error does not say the default is at fault: %s", q, msg)
		}
	}
	// The default belongs to the document, so supplying the variable does not
	// make the document valid.
	if resp := run(t, e, `query($l: Int = 99999999999) { f(l: $l) }`, `{"l": 3}`); len(resp.Errors) != 1 || resp.Data != nil {
		t.Fatalf("a supplied value excused an invalid default: data=%s errors=%s", resp.Data, errorsJSON(resp.Errors))
	}
	expectData(t, run(t, e, `query($l: Int = 7) { f(l: $l) }`, ""), `{"f":7}`)
}

// Coercion validates the variables and returns the map the executor reads. It
// built that map afresh on every request, entry by entry, when in the ordinary
// case -- every declared variable supplied and nothing else -- it is the map
// that was just decoded.
func TestCoerceVariablesReusesTheDecodedMapWhenNothingDiffers(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { f(a: Int, b: String): Int }`),
		Query(FieldArgs("f", func(Root, struct {
			A *int
			B *string
		}) *int {
			return nil
		})))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	op := func(q string) *ast.OperationDefinition {
		entry, errs := NewExecutor(s).document(q)
		if errs != nil {
			t.Fatalf("document: %v", errs[0])
		}
		return entry.doc.Operations[0]
	}
	same := func(a, b map[string]any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	const q = `query($a: Int, $b: String = "d") { f(a: $a, b: $b) }`

	raw := map[string]any{"a": json.Number("1"), "b": "x"}
	out, cerr := s.coerceVariables(op(q), raw)
	if cerr != nil || !same(out, raw) {
		t.Fatalf("every variable supplied and nothing else: want the decoded map itself (err %v)", cerr)
	}

	// Anything that makes the result differ still gets a map of its own.
	for name, raw := range map[string]map[string]any{
		"an undeclared variable is dropped":        {"a": json.Number("1"), "b": "x", "extra": true},
		"an absent variable takes its default":     {"a": json.Number("1")},
		"an absent nullable variable stays absent": {"b": "x"},
	} {
		out, cerr := s.coerceVariables(op(q), raw)
		if cerr != nil {
			t.Fatalf("%s: %v", name, cerr)
		}
		if same(out, raw) {
			t.Errorf("%s: the decoded map was returned unchanged", name)
		}
		if _, ok := out["extra"]; ok {
			t.Errorf("%s: an undeclared variable reached the executor", name)
		}
	}
	if out, _ := s.coerceVariables(op(q), map[string]any{"a": json.Number("1")}); out["b"] != "d" {
		t.Errorf("default not applied: %v", out)
	}
	if out, _ := s.coerceVariables(op(q), map[string]any{"b": "x"}); len(out) != 1 {
		t.Errorf("an absent variable with no default must stay absent: %v", out)
	}
}

// @skip and @include take a Boolean!, and a variable with a default may be
// declared nullable there, so a request can still send it as null. That is a
// null for a non-null argument: graphql-js refuses the operation. Read as
// "not true", it ran the request with the field included, or excluded, and
// said nothing.
func TestANullConditionVariableIsARequestError(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for name, q := range map[string]string{
		"skip on a field":             `query($b: Boolean = true) { strings @skip(if: $b) ctxPath }`,
		"include on a field":          `query($b: Boolean = false) { strings @include(if: $b) ctxPath }`,
		"on a fragment spread":        `query($b: Boolean = true) { ...F @skip(if: $b) ctxPath } fragment F on Query { strings }`,
		"on an inline fragment":       `query($b: Boolean = true) { ... @include(if: $b) { strings } ctxPath }`,
		"inside a fragment":           `query($b: Boolean = true) { ...F ctxPath } fragment F on Query { strings @skip(if: $b) }`,
		"beneath a field":             `query($b: Boolean = true) { me { id @skip(if: $b) name } }`,
		"in a fragment under a field": `query($b: Boolean = true) { me { ...U } } fragment U on User { id @skip(if: $b) name }`,
	} {
		t.Run(name, func(t *testing.T) {
			// A request with a value first, so the null arrives at a plan the
			// cache already holds: the check cannot live in the compile.
			if resp := run(t, e, q, `{"b":false}`); len(resp.Errors) > 0 {
				t.Fatalf("b=false: %s", errorsJSON(resp.Errors))
			}
			resp := run(t, e, q, `{"b":null}`)
			if !resp.HasRequestErrors() {
				t.Fatalf("a null condition ran the operation: data=%s errors=%s", resp.Data, errorsJSON(resp.Errors))
			}
			err := resp.Errors[0]
			if !strings.Contains(err.Message, `"$b"`) || err.Extensions["code"] != CodeBadUserInput || len(err.Locations) == 0 {
				t.Fatalf("error = %s", errorsJSON(resp.Errors))
			}
			// The default still applies when the variable is left out.
			if resp := run(t, e, q, ""); len(resp.Errors) > 0 {
				t.Fatalf("no variables: %s", errorsJSON(resp.Errors))
			}
		})
	}
}

// The refusal is for a condition, not for a null: a nullable variable that
// this operation hands to an ordinary argument may be null, even when another
// operation of the document uses the same name in a directive.
func TestANullVariableOutsideAConditionIsStillAccepted(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const q = `query A($b: Boolean = true) { echo(b: $b) } query B($b: Boolean = true) { strings @skip(if: $b) }`
	resp := e.Execute(t.Context(), &Request{Query: q, OperationName: "A", Variables: []byte(`{"b":null}`)})
	expectData(t, resp, `{"echo":"{\"V\":null,\"S\":\"d\",\"F\":null,\"B\":null,\"List\":null}"}`)
	resp = e.Execute(t.Context(), &Request{Query: q, OperationName: "B", Variables: []byte(`{"b":null}`)})
	if !resp.HasRequestErrors() {
		t.Fatalf("operation B ran with a null condition: %s", resp.Data)
	}
}

// A document past maxCondVars takes the uncached path, which reads the
// conditions separately; the refusal has to hold there too.
func TestANullConditionVariableIsRefusedOnTheUncachedPath(t *testing.T) {
	_, e := newFixtureExecutor(t)
	var decl, body, vars []string
	for i := range maxCondVars + 1 {
		decl = append(decl, fmt.Sprintf("$c%d: Boolean = false", i))
		body = append(body, fmt.Sprintf("s%d: strings @skip(if: $c%d)", i, i))
		vars = append(vars, fmt.Sprintf(`"c%d": false`, i))
	}
	q := "query(" + strings.Join(decl, ", ") + ") { " + strings.Join(body, " ") + " }"
	if resp := run(t, e, q, "{"+strings.Join(vars, ",")+"}"); len(resp.Errors) > 0 {
		t.Fatalf("all false: %s", errorsJSON(resp.Errors))
	}
	vars[len(vars)-1] = fmt.Sprintf(`"c%d": null`, maxCondVars)
	if resp := run(t, e, q, "{"+strings.Join(vars, ",")+"}"); !resp.HasRequestErrors() {
		t.Fatalf("a null condition ran the operation: %s", resp.Data)
	}
}

type deepWhere struct {
	Name  *string
	Not   *deepWhere
	Items []*deepWhere
}

type deepWhereArgs struct{ F *deepWhere }

// A variable's error names where in the value it is, and that path used to be
// built on the way down: a string a level, each a copy of the last and a
// little longer, whether or not anything turned out to be wrong. So refusing
// a value nested as deep as the variables decoder admits -- an 80 KB body --
// allocated 221 MB, quadratic in a depth the client chooses. The path is now
// written once, for the error that is reported, and what it says is the same.
func TestAnInvalidVariableIsRefusedInSpaceLinearInItsDepth(t *testing.T) {
	s, err := NewSchema(SDL(`
		input Where { name: String, not: Where, items: [Where!] }
		type Query { q(f: Where): String }
	`),
		Input[deepWhere]("Where"),
		Query(ResolveArgs("q", func(context.Context, Root, deepWhereArgs) (string, error) { return "", nil })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	const query = `query($f: Where) { q(f: $f) }`
	refusal := func(vars string) string {
		t.Helper()
		resp := run(t, e, query, vars)
		if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeBadUserInput || resp.Data != nil {
			t.Fatalf("want one BAD_USER_INPUT and no data, got %.300s", errorsJSON(resp.Errors))
		}
		return resp.Errors[0].Message
	}

	// The wording, where it is short enough to write out: fields, list
	// indexes and both together, as they were.
	for vars, want := range map[string]string{
		`{"f":{"name":5}}`:                                    `Variable "$f" got invalid value {"name":5}; String cannot represent a non string value: 5 at $f.name.`,
		`{"f":{"not":{"not":{"name":5}}}}`:                    `Variable "$f" got invalid value {"not":{"not":{"name":5}}}; String cannot represent a non string value: 5 at $f.not.not.name.`,
		`{"f":{"items":[{},{"not":{"items":[{"name":5}]}}]}}`: `Variable "$f" got invalid value {"items":[{},{"not":{"items":[{"name":5}]}}]}; String cannot represent a non string value: 5 at $f.items[1].not.items[0].name.`,
		`{"f":{"items":[{},null]}}`:                           `Variable "$f" got invalid value {"items":[{},null]}; Expected non-nullable type "Where!" not to be null at $f.items[1].`,
		`{"f":{"not":{"nope":1}}}`:                            `Variable "$f" got invalid value {"not":{"nope":1}}; Field "nope" is not defined by type "Where" at $f.not.`,
	} {
		if got := refusal(vars); got != want {
			t.Errorf("%s:\n got %s\nwant %s", vars, got, want)
		}
	}

	const depth = 9900
	vars := `{"f":` + strings.Repeat(`{"not":`, depth) + `{"name":5}` + strings.Repeat("}", depth) + "}"
	var message string
	got := allocatedBytes(func() { message = refusal(vars) })
	if want := "String cannot represent a non string value: 5 at $f" + strings.Repeat(".not", depth) + ".name."; !strings.HasSuffix(message, want) {
		t.Fatalf("the error does not end with the path to the value: ...%s", message[max(0, len(message)-120):])
	}
	// Decoding the body, and the one path the error carries, are a few
	// megabytes; a path a level was two hundred.
	if got > 16<<20 {
		t.Fatalf("refusing a %d-byte variable nested %d deep allocated %d MB", len(vars), depth, got>>20)
	}
}
