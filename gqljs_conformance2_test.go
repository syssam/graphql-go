package graphql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The second recorded case set: abstract types with two real implementations,
// the whole list-nullability matrix, nested input coercion and directives on
// fragments. testdata/gqljs/run2.mjs is the graphql-js side; this mirrors it.
//
// Round one reported five differences that were all bugs in the Go fixture, so
// the two schemas are written to be read side by side.

const gqljs2SDL = `
enum Size { SMALL LARGE }
input Inner { n: Int  s: String = "inner-default" }
input Outer {
  inner: Inner
  list: [Inner]
  size: Size = SMALL
  req: Int! = 7
}
interface Named { name: String! }
type Dog implements Named { name: String!  barks: Boolean! }
type Cat implements Named { name: String!  lives: Int! }
union Pet = Dog | Cat
type Query {
  nn: [String]
  nx: [String!]
  xn: [String]!
  xx: [String!]!
  nnNull: [String]
  xnNull: [String]!
  deepNN: [[String]]
  deepXX: [[String!]!]!
  named: Named
  namedCat: Named
  pet: Pet
  namedList: [Named]
  outerArg(v: Outer): String
  innerListArg(v: [Inner]): String
  sizeArg(v: Size = LARGE): String
  str: String!
  boom: String
}
`

type jsDog struct {
	Name  string
	Barks bool
}

type jsCat struct {
	Name  string
	Lives int
}

// Omittable on every input field, because absent and null are different
// answers and telling them apart is most of what this set compares.
type jsInner struct {
	N Omittable[*int]
	S Omittable[*string]
}

type jsOuter struct {
	Inner Omittable[*jsInner]
	List  Omittable[[]*jsInner]
	Size  Omittable[*string]
	Req   Omittable[*int]
}

type jsOuterArgs struct{ V Omittable[*jsOuter] }
type jsInnerListArgs struct{ V Omittable[[]*jsInner] }
type jsSizeArgs struct{ V Omittable[*string] }

// jsScalar and the three formatters below are a line-for-line port of the
// helpers in run2.mjs. Comparing a Go struct against JSON.stringify would
// compare serialization rather than coercion: a struct cannot omit a field,
// and "absent" is exactly the answer under test.
func jsScalar[T any](o Omittable[*T]) string {
	switch {
	case !o.IsSet():
		return "absent"
	case o.Value() == nil:
		return "null"
	}
	return fmt.Sprint(*o.Value())
}

func jsFmtInner(v *jsInner) string {
	if v == nil {
		return "null"
	}
	return "Inner{n=" + jsScalar(v.N) + ",s=" + jsScalar(v.S) + "}"
}

func jsFmtInnerOpt(o Omittable[*jsInner]) string {
	if !o.IsSet() {
		return "absent"
	}
	return jsFmtInner(o.Value())
}

func jsFmtInnerList(o Omittable[[]*jsInner]) string {
	if !o.IsSet() {
		return "absent"
	}
	if o.Value() == nil {
		return "null"
	}
	parts := make([]string, 0, len(o.Value()))
	for _, e := range o.Value() {
		parts = append(parts, jsFmtInner(e))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func jsFmtOuter(o Omittable[*jsOuter]) string {
	if !o.IsSet() {
		return "absent"
	}
	v := o.Value()
	if v == nil {
		return "null"
	}
	return "Outer{inner=" + jsFmtInnerOpt(v.Inner) +
		",list=" + jsFmtInnerList(v.List) +
		",size=" + jsScalar(v.Size) +
		",req=" + jsScalar(v.Req) + "}"
}

func newGQLJSExecutor2(t *testing.T) *Executor {
	t.Helper()
	boom := errors.New("boom")
	sp := func(s string) *string { return &s }
	// ["a", null, "c"], the input to every row of the nullability matrix.
	abc := func() []*string { return []*string{sp("a"), nil, sp("c")} }

	s, err := NewSchema(SDL(gqljs2SDL),
		Interface[any]("Named"),
		Union[any]("Pet"),
		Object[jsDog]("Dog",
			Field("name", func(d *jsDog) string { return d.Name }),
			Field("barks", func(d *jsDog) bool { return d.Barks }),
		),
		Object[jsCat]("Cat",
			Field("name", func(c *jsCat) string { return c.Name }),
			Field("lives", func(c *jsCat) int { return c.Lives }),
		),
		Enum[string]("Size", map[string]string{"SMALL": "SMALL", "LARGE": "LARGE"}),
		Input[jsInner]("Inner",
			OmittableField("n", func(i *jsInner, v Omittable[*int]) { i.N = v }),
			OmittableField("s", func(i *jsInner, v Omittable[*string]) { i.S = v }),
		),
		Input[jsOuter]("Outer",
			OmittableField("inner", func(o *jsOuter, v Omittable[*jsInner]) { o.Inner = v }),
			OmittableField("list", func(o *jsOuter, v Omittable[[]*jsInner]) { o.List = v }),
			OmittableField("size", func(o *jsOuter, v Omittable[*string]) { o.Size = v }),
			OmittableField("req", func(o *jsOuter, v Omittable[*int]) { o.Req = v }),
		),
		Args[jsOuterArgs](OmittableField("v", func(a *jsOuterArgs, v Omittable[*jsOuter]) { a.V = v })),
		Args[jsInnerListArgs](OmittableField("v", func(a *jsInnerListArgs, v Omittable[[]*jsInner]) { a.V = v })),
		Args[jsSizeArgs](OmittableField("v", func(a *jsSizeArgs, v Omittable[*string]) { a.V = v })),
		Query(
			Resolve("nn", func(context.Context, Root) ([]*string, error) { return abc(), nil }),
			Resolve("nx", func(context.Context, Root) ([]*string, error) { return abc(), nil }),
			Resolve("xn", func(context.Context, Root) ([]*string, error) { return abc(), nil }),
			Resolve("xx", func(context.Context, Root) ([]*string, error) { return abc(), nil }),
			Resolve("nnNull", func(context.Context, Root) ([]*string, error) { return nil, nil }),
			Resolve("xnNull", func(context.Context, Root) ([]*string, error) { return nil, nil }),
			Resolve("deepNN", func(context.Context, Root) ([][]*string, error) {
				return [][]*string{{sp("a")}, {nil}, nil}, nil
			}),
			Resolve("deepXX", func(context.Context, Root) ([][]*string, error) {
				return [][]*string{{sp("a")}, {nil}, {sp("c")}}, nil
			}),
			Resolve("named", func(context.Context, Root) (any, error) {
				return &jsDog{Name: "Rex", Barks: true}, nil
			}),
			Resolve("namedCat", func(context.Context, Root) (any, error) {
				return &jsCat{Name: "Tom", Lives: 9}, nil
			}),
			Resolve("pet", func(context.Context, Root) (any, error) {
				return &jsDog{Name: "Rex", Barks: true}, nil
			}),
			Resolve("namedList", func(context.Context, Root) ([]any, error) {
				return []any{&jsDog{Name: "Rex", Barks: true}, &jsCat{Name: "Tom", Lives: 9}}, nil
			}),
			ResolveArgs("outerArg", func(_ context.Context, _ Root, a jsOuterArgs) (*string, error) {
				return sp(jsFmtOuter(a.V)), nil
			}),
			ResolveArgs("innerListArg", func(_ context.Context, _ Root, a jsInnerListArgs) (*string, error) {
				return sp(jsFmtInnerList(a.V)), nil
			}),
			ResolveArgs("sizeArg", func(_ context.Context, _ Root, a jsSizeArgs) (*string, error) {
				return sp(jsScalar(a.V)), nil
			}),
			Field("str", func(Root) string { return "ok" }),
			Resolve("boom", func(context.Context, Root) (*string, error) { return nil, boom }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}
