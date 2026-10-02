package graphql

import (
	"slices"
	"testing"
)

type sharedListArgs struct{ List []int }

type sharedPage struct{ Limit int }

type sharedPageArgs struct{ Page *sharedPage }

type sharedScalarArgs struct {
	First *int
	Name  string
}

func sharedArgsExecutor(t *testing.T) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`
		input Page { limit: Int! = 0 }
		type Query {
			first(list: [Int!]!): Int!
			limit(page: Page): Int!
			scalar(first: Int, name: String! = "n"): Int!
		}`),
		Args[sharedListArgs](),
		Input[sharedPage]("Page"),
		Args[sharedPageArgs](),
		Args[sharedScalarArgs](),
		Query(
			// Both resolvers do what resolvers do with arguments of their own:
			// normalise them in place.
			FieldArgs("first", func(_ Root, a sharedListArgs) int {
				head := a.List[0]
				slices.Sort(a.List)
				return head
			}),
			FieldArgs("limit", func(_ Root, a sharedPageArgs) int {
				if a.Page.Limit == 0 {
					a.Page.Limit = 20
					return 0
				}
				return a.Page.Limit
			}),
			FieldArgs("scalar", func(_ Root, a sharedScalarArgs) int { return *a.First }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

// Literal arguments are decoded once at plan compile and the plan is shared,
// so a slice or a nested input object in them was one value handed to every
// request: a resolver that sorted or defaulted its own arguments changed the
// next request's, and two at once raced. The same query sent with variables
// decoded per request and did neither.
func TestLiteralArgumentsAreNotSharedBetweenRequests(t *testing.T) {
	e := sharedArgsExecutor(t)
	for _, tc := range []struct{ name, query, want string }{
		{"list", `{ first(list: [3, 1, 2]) }`, `{"first":3}`},
		{"input object", `{ limit(page: {}) }`, `{"limit":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectData(t, run(t, e, tc.query, ""), tc.want)
			expectData(t, run(t, e, tc.query, ""), tc.want)
		})
	}
}

// What the fix must not cost: arguments that hold nothing a resolver can
// reach into are still decoded once, at plan compile.
func TestScalarLiteralArgumentsAreStillDecodedOnce(t *testing.T) {
	e := sharedArgsExecutor(t)
	for query, wantPerRequest := range map[string]bool{
		`{ scalar(first: 2, name: "x") }`: false,
		`{ first(list: [1]) }`:            true,
		`{ limit(page: {limit: 1}) }`:     true,
		// The type can hold a nested object; this query's value does not.
		`{ limit }`: false,
	} {
		oc, errs := e.prepareOperation(&Request{Query: query}, false)
		if errs != nil {
			t.Fatalf("%s: %v", query, errs[0])
		}
		f := oc.plan.sel.fields[0]
		if f.dynamicArgs != wantPerRequest {
			t.Errorf("%s: decoded per request = %t, want %t", query, f.dynamicArgs, wantPerRequest)
		}
		if !wantPerRequest && f.args == nil {
			t.Errorf("%s: no arguments were decoded at plan compile", query)
		}
	}
}
