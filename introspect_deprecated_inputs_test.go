package graphql

import (
	"context"
	"encoding/json"
	"testing"
)

const deprecatedInputsSDL = `
input Filter { keep: Int old: Int @deprecated(reason: "use keep") }
type Query { f(x: Int, y: Int @deprecated(reason: "use x")): Int }
`

func deprecatedInputsSchema(t *testing.T, opts ...SchemaOption) *Executor {
	t.Helper()
	opts = append(opts,
		Input[struct {
			Keep *int `graphql:"keep"`
			Old  *int `graphql:"old"`
		}]("Filter"),
		Args[struct {
			X *int `graphql:"x"`
			Y *int `graphql:"y"`
		}](),
		Query(ResolveArgs("f", func(_ context.Context, _ Root, a struct {
			X *int `graphql:"x"`
			Y *int `graphql:"y"`
		}) (*int, error) {
			return nil, nil
		})))
	s, err := NewSchema(SDL(deprecatedInputsSDL), opts...)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s)
}

func deprecatedNames(t *testing.T, exec *Executor, q string) (inputs, args []string) {
	t.Helper()
	r := exec.Execute(context.Background(), &Request{Query: q})
	if len(r.Errors) != 0 {
		t.Fatalf("%s: %v", q, r.Errors)
	}
	var out struct {
		Filter struct {
			InputFields []struct{ Name string } `json:"inputFields"`
		} `json:"filter"`
		Query struct {
			Fields []struct {
				Name string
				Args []struct{ Name string }
			} `json:"fields"`
		} `json:"query"`
	}
	if err := json.Unmarshal(r.Data, &out); err != nil {
		t.Fatal(err)
	}
	for _, f := range out.Filter.InputFields {
		inputs = append(inputs, f.Name)
	}
	for _, f := range out.Query.Fields {
		if f.Name == "f" {
			for _, a := range f.Args {
				args = append(args, a.Name)
			}
		}
	}
	return
}

const introDefault = `{ filter: __type(name: "Filter") { inputFields { name } } query: __type(name: "Query") { fields { name args { name } } } }`
const introExplicitFalse = `{ filter: __type(name: "Filter") { inputFields(includeDeprecated: false) { name } } query: __type(name: "Query") { fields { name args(includeDeprecated: false) { name } } } }`

func sameNames(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || a[0] == b[0] && (len(a) < 2 || a[1] == b[1]))
}

// The specification hides a deprecated input field or argument unless the client asks for it.
func TestDeprecatedInputValuesAreHiddenByDefault(t *testing.T) {
	in, args := deprecatedNames(t, deprecatedInputsSchema(t), introDefault)
	if !sameNames(in, []string{"keep"}) || !sameNames(args, []string{"x"}) {
		t.Fatalf("inputFields=%v args=%v, want only the non-deprecated ones", in, args)
	}
}

// gqlgen returned them whatever the client asked, and a client that never passes
// includeDeprecated (graphql-js's getIntrospectionQuery does not) relies on that.
func TestIntrospectDeprecatedInputValuesRestoresThatDefault(t *testing.T) {
	exec := deprecatedInputsSchema(t, IntrospectDeprecatedInputValues())
	in, args := deprecatedNames(t, exec, introDefault)
	if !sameNames(in, []string{"keep", "old"}) || !sameNames(args, []string{"x", "y"}) {
		t.Fatalf("inputFields=%v args=%v, want the deprecated ones too", in, args)
	}
	// an explicit false is still honoured: the option changes the default, not the client's say
	in, args = deprecatedNames(t, exec, introExplicitFalse)
	if !sameNames(in, []string{"keep"}) || !sameNames(args, []string{"x"}) {
		t.Fatalf("explicit includeDeprecated:false: inputFields=%v args=%v", in, args)
	}
}
