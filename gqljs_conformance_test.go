package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
)

// The reference-implementation differential. testdata/gqljs/expected.json is
// graphql-js 17.0.2's own output for testdata/gqljs/cases.json, recorded by
// testdata/gqljs/run.mjs; the schema below mirrors the one that script builds.
//
// Asserted: the response shape, which fields went null, how far a non-null
// error bubbled, and the path on every error, in order.
//
// Message wording is recorded but not compared. Seven of the eight that differ
// come out of gqlparser, which still carries graphql-js 16's phrasing, and
// message text is not specified; testdata/gqljs/README.md has the table.
//
// Error order was compared as a multiset until the engine was changed to report
// in document order, which is the one behavioural difference this differential
// found. TestErrorsAreInDocumentOrder holds that directly.

const gqljsSDL = `
enum Color { RED GREEN }
input Inp { n: Int! }
interface Node { id: ID! }
type Thing implements Node { id: ID! }
type Other implements Node { id: ID! }
type Obj { failNonNull: String! ok: String }
type Query {
  str: String!
  failNullable: String
  failNonNull: String!
  nullFromNonNull: String!
  nullableObj: Obj
  node: Node
  listNullableElems: [String]
  listNonNullElems: [String!]
  grid: [[String!]]
  withDefault(v: String = "D"): String
  intArg(v: Int): String
  floatArg(v: Float): String
  idArg(v: ID): String
  enumArg(v: Color): String
  inputArg(v: Inp): String
  listArg(v: [Int]): String
}
type Mutation { bump: String! }
`

type jsThing struct{ ID ID }
type jsOther struct{ ID ID }
type jsObj struct{}
type jsInp struct{ N int }

type jsDefArgs struct{ V Omittable[*string] }
type jsIntArgs struct{ V *int }
type jsFloatArgs struct{ V *float64 }
type jsIDArgs struct{ V *ID }
type jsEnumArgs struct{ V *string }
type jsInpArgs struct{ V *jsInp }
type jsListArgs struct{ V []*int }

type gqljsCase struct {
	Name      string         `json:"name"`
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type gqljsExpect struct {
	Name       string          `json:"name"`
	HasData    bool            `json:"hasData"`
	Data       json.RawMessage `json:"data"`
	ErrorCount int             `json:"errorCount"`
	ErrorPaths [][]any         `json:"errorPaths"`
	Messages   []string        `json:"messages"`
}

func newGQLJSExecutor(t *testing.T) *Executor {
	t.Helper()
	boom := errors.New("boom")
	str := func(v any) *string {
		s := fmt.Sprint(v)
		return &s
	}
	s, err := NewSchema(SDL(gqljsSDL),
		Interface[any]("Node"),
		Object[jsThing]("Thing", Field("id", func(t *jsThing) ID { return t.ID })),
		Object[jsOther]("Other", Field("id", func(t *jsOther) ID { return t.ID })),
		Object[jsObj]("Obj",
			Resolve("failNonNull", func(context.Context, *jsObj) (string, error) { return "", boom }),
			Field("ok", func(*jsObj) *string { return str("x") }),
		),
		Args[jsDefArgs](OmittableField("v", func(a *jsDefArgs, v Omittable[*string]) { a.V = v })),
		Args[jsIntArgs](InputField("v", func(a *jsIntArgs, v *int) { a.V = v })),
		Args[jsFloatArgs](InputField("v", func(a *jsFloatArgs, v *float64) { a.V = v })),
		Args[jsIDArgs](InputField("v", func(a *jsIDArgs, v *ID) { a.V = v })),
		Args[jsEnumArgs](InputField("v", func(a *jsEnumArgs, v *string) { a.V = v })),
		Args[jsInpArgs](InputField("v", func(a *jsInpArgs, v *jsInp) { a.V = v })),
		Args[jsListArgs](InputField("v", func(a *jsListArgs, v []*int) { a.V = v })),
		Enum[string]("Color", map[string]string{"RED": "RED", "GREEN": "GREEN"}),
		Input[jsInp]("Inp", InputField("n", func(i *jsInp, v int) { i.N = v })),
		Query(
			Field("str", func(Root) string { return "ok" }),
			Resolve("failNullable", func(context.Context, Root) (*string, error) { return nil, boom }),
			Resolve("failNonNull", func(context.Context, Root) (string, error) { return "", boom }),
			// A nil *string is the only way a Go signature can hand null to a
			// String! field, which graphql-js writes as `resolve: () => null`.
			Resolve("nullFromNonNull", func(context.Context, Root) (*string, error) { return nil, nil }),
			Field("nullableObj", func(Root) *jsObj { return &jsObj{} }),
			Resolve("node", func(context.Context, Root) (any, error) { return &jsThing{ID: "1"}, nil }),
			Resolve("listNullableElems", func(context.Context, Root) ([]*string, error) { return nil, boom }),
			Resolve("listNonNullElems", func(context.Context, Root) ([]*string, error) {
				return []*string{str("a"), nil, str("c")}, nil
			}),
			Resolve("grid", func(context.Context, Root) ([][]*string, error) {
				return [][]*string{{str("a")}, {nil}, {str("c")}}, nil
			}),
			ResolveArgs("withDefault", func(_ context.Context, _ Root, a jsDefArgs) (*string, error) {
				switch {
				case !a.V.IsSet():
					return str("ABSENT"), nil
				case a.V.Value() == nil:
					return str("NULL"), nil
				}
				return a.V.Value(), nil
			}),
			ResolveArgs("intArg", func(_ context.Context, _ Root, a jsIntArgs) (*string, error) {
				return jsDeref(a.V), nil
			}),
			ResolveArgs("floatArg", func(_ context.Context, _ Root, a jsFloatArgs) (*string, error) {
				return jsDeref(a.V), nil
			}),
			ResolveArgs("idArg", func(_ context.Context, _ Root, a jsIDArgs) (*string, error) {
				return jsDeref(a.V), nil
			}),
			ResolveArgs("enumArg", func(_ context.Context, _ Root, a jsEnumArgs) (*string, error) {
				return jsDeref(a.V), nil
			}),
			ResolveArgs("inputArg", func(_ context.Context, _ Root, a jsInpArgs) (*string, error) {
				b, err := json.Marshal(a.V)
				return str(string(b)), err
			}),
			ResolveArgs("listArg", func(_ context.Context, _ Root, a jsListArgs) (*string, error) {
				b, err := json.Marshal(a.V)
				return str(string(b)), err
			}),
		),
		Mutation(Field("bump", func(Root) string { return "bumped" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

func jsDeref[T any](v *T) *string {
	var s string
	if v == nil {
		s = "<nil>"
	} else {
		s = fmt.Sprint(*v)
	}
	return &s
}

func readGQLJSON[T any](t *testing.T, name string) []T {
	t.Helper()
	b, err := os.ReadFile("testdata/gqljs/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out []T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return out
}

// TestGraphQLJSDifferential runs every recorded case set and requires this
// engine to agree with graphql-js on all of it.
func TestGraphQLJSDifferential(t *testing.T) {
	for _, set := range []struct {
		name, cases, expected string
		exec                  func(*testing.T) *Executor
	}{
		{"core", "cases.json", "expected.json", newGQLJSExecutor},
		{"abstract-lists-inputs", "cases2.json", "expected2.json", newGQLJSExecutor2},
	} {
		t.Run(set.name, func(t *testing.T) {
			runGQLJSSet(t, set.exec(t), set.cases, set.expected)
		})
	}
}

func runGQLJSSet(t *testing.T, e *Executor, casesFile, expectedFile string) {
	t.Helper()
	cases := readGQLJSON[gqljsCase](t, casesFile)
	expects := readGQLJSON[gqljsExpect](t, expectedFile)
	if len(cases) != len(expects) {
		t.Fatalf("%s has %d cases, %s has %d", casesFile, len(cases), expectedFile, len(expects))
	}
	want := make(map[string]gqljsExpect, len(expects))
	for _, x := range expects {
		want[x.Name] = x
	}

	for _, c := range cases {
		x, ok := want[c.Name]
		if !ok {
			t.Fatalf("case %q has no recorded graphql-js result", c.Name)
		}
		if d, ok := intendedDivergences[c.Name]; ok {
			t.Run(c.Name, func(t *testing.T) {
				resp := e.Execute(context.Background(), &Request{Query: c.Query})
				if got := string(resp.Data); got != d.data || len(resp.Errors) != 0 {
					t.Errorf("intended divergence changed: data %s, errors %s; want %s and none", got, errorsJSON(resp.Errors), d.data)
				}
			})
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			req := &Request{Query: c.Query}
			if c.Variables != nil {
				vb, err := json.Marshal(c.Variables)
				if err != nil {
					t.Fatalf("marshal variables: %v", err)
				}
				req.Variables = vb
			}
			resp := e.Execute(context.Background(), req)

			if got := resp.Data != nil; got != x.HasData {
				t.Errorf("data present = %v, graphql-js = %v", got, x.HasData)
			}
			gotData := "null"
			if resp.Data != nil {
				gotData = string(resp.Data)
			}
			if wantData := jsCanonical(t, x.Data); gotData != wantData {
				t.Errorf("data\n got %s\n  js %s", gotData, wantData)
			}
			if got := len(resp.Errors); got != x.ErrorCount {
				t.Errorf("error count = %d, graphql-js = %d (%s)", got, x.ErrorCount, errorsJSON(resp.Errors))
			}
			if got, w := jsPaths(resp), jsWantPaths(t, x.ErrorPaths); !slices.Equal(got, w) {
				t.Errorf("error paths\n got %s\n  js %s", got, w)
			}
		})
	}
}

// jsCanonical strips insignificant whitespace from the recorded data and
// nothing else. It must not round-trip through map[string]any: that sorts the
// keys, and response key order is document order, which the specification
// requires and this comparison is here to check. It did round-trip at first,
// and the first case set passed either way only because its field names
// happened to be alphabetical.
func jsCanonical(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return "null"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact recorded data: %v", err)
	}
	return buf.String()
}

func jsPaths(resp *Response) []string {
	out := make([]string, 0, len(resp.Errors))
	for _, e := range resp.Errors {
		if len(e.Path) == 0 {
			continue
		}
		p := make([]any, 0, len(e.Path))
		for _, seg := range e.Path {
			if seg.IsIndex {
				p = append(p, seg.Index)
			} else {
				p = append(p, seg.Key)
			}
		}
		b, _ := json.Marshal(p)
		out = append(out, string(b))
	}
	return out
}

func jsWantPaths(t *testing.T, paths [][]any) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if len(p) == 0 {
			continue
		}
		// JSON numbers decode as float64; the engine reports int indices.
		seg := make([]any, 0, len(p))
		for _, s := range p {
			if f, ok := s.(float64); ok {
				seg = append(seg, int(f))
				continue
			}
			seg = append(seg, s)
		}
		b, err := json.Marshal(seg)
		if err != nil {
			t.Fatalf("encode recorded paths: %v", err)
		}
		out = append(out, string(b))
	}
	return out
}

// intendedDivergences are the recorded cases this engine answers differently
// from graphql-js on purpose. Each is asserted exactly, so a change in either
// direction is a failing test, not a silent drift.
var intendedDivergences = map[string]struct {
	data, why string
}{
	// graphql-js: `resolve: () => null` for [String]! is a null violation.
	// A Go resolver returning a slice has no null to give: nil is the empty
	// list (see TestNilSliceAtNonNullListIsEmpty), and the engine does not
	// bind *[]T. So the Go fixture's nil is an empty list and answers [];
	// refusing a list is what returning an error is for.
	"whole list null, [String]!": {
		data: `{"xnNull":[]}`,
		why:  "a nil Go slice is the empty list at a non-null list position",
	},
}
