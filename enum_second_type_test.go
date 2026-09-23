package graphql

import (
	"context"
	"strings"
	"testing"
)

// The registry is keyed on (GraphQL type, reflect.Type), so one SDL enum can
// be backed by two Go types at once. That is not a curiosity: an ent-derived
// ORM generates one enum type beside the entity and another beside the column,
// and a schema binds the first while its own filter inputs carry the second.
// codegen discovers the pair (`altEnumTypes`); this is the engine half, and
// without it that discovery would be emitting an error.

type entMethod string

const (
	entMethodStraight entMethod = "straight_line"
	entMethodNone     entMethod = "none"
)

type colMethod string

const (
	colMethodStraight colMethod = "straight_line"
	colMethodNone     colMethod = "none"
)

type methodFilter struct {
	Method *colMethod
}

type depreciation struct {
	Method entMethod
}

const secondTypeSDL = `
enum Method { STRAIGHT_LINE NONE }
input MethodFilter { method: Method }
type Depreciation { method: Method! }
type Query { find(filter: MethodFilter!): Depreciation! }
`

type findArgs struct{ Filter methodFilter }

func newSecondTypeSchema(t *testing.T, opts ...SchemaOption) (*Schema, error) {
	t.Helper()
	base := []SchemaOption{
		Object[depreciation]("Depreciation",
			Field("method", func(d *depreciation) entMethod { return d.Method }),
		),
		Input[methodFilter]("MethodFilter"),
		Args[findArgs](InputField("filter", func(a *findArgs, v methodFilter) { a.Filter = v })),
		Query(FieldArgs("find", func(_ Root, a findArgs) *depreciation {
			if a.Filter.Method == nil {
				return &depreciation{Method: entMethodNone}
			}
			return &depreciation{Method: entMethod(*a.Filter.Method)}
		})),
	}
	return NewSchema(SDL(secondTypeSDL), append(base, opts...)...)
}

func TestEnumAcceptsASecondGoType(t *testing.T) {
	s, err := newSecondTypeSchema(t,
		Enum[entMethod]("Method", map[entMethod]string{
			entMethodStraight: "STRAIGHT_LINE",
			entMethodNone:     "NONE",
		}),
		Enum[colMethod]("Method", map[colMethod]string{
			colMethodStraight: "STRAIGHT_LINE",
			colMethodNone:     "NONE",
		}),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := NewExecutor(s).Execute(context.Background(), &Request{
		Query: `{ find(filter: {method: STRAIGHT_LINE}) { method } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("execute: %v", resp.Errors[0])
	}
	// The input decoded into colMethod and the output wrote from entMethod.
	if got := string(resp.Data); !strings.Contains(got, `"STRAIGHT_LINE"`) {
		t.Fatalf("data = %s", got)
	}
}

// Binding only the type the object uses is the state codegen produced before
// it discovered the second one, and it is a build error naming the position.
func TestEnumWithOneGoTypeRejectsTheOther(t *testing.T) {
	_, err := newSecondTypeSchema(t,
		Enum[entMethod]("Method", map[entMethod]string{
			entMethodStraight: "STRAIGHT_LINE",
			entMethodNone:     "NONE",
		}),
	)
	if err == nil {
		t.Fatal("NewSchema accepted an input field typed by an unbound Go enum")
	}
	if !strings.Contains(err.Error(), "MethodFilter.method") {
		t.Fatalf("error does not name the input field: %v", err)
	}
}
