package graphql

import (
	"context"
	"testing"
)

// Input objects arriving as variables, shaped like the WhereInput an ent or
// velox schema has one of per entity: recursive through not/and/or, and wide
// -- 123 fields, of which a request sends two. BenchmarkExecuteVariableArgs
// reaches argument decoding with three scalars and no input object at all.

type benchWhere struct {
	Not          *benchWhere   `json:"not"`
	And          []*benchWhere `json:"and"`
	Or           []*benchWhere `json:"or"`
	Name         *string       `json:"name"`
	NameNEQ      *string       `json:"nameNEQ"`
	NameIn       []string      `json:"nameIn"`
	NameNotIn    []string      `json:"nameNotIn"`
	NameContains *string       `json:"nameContains"`
}

type benchWide struct {
	Not            *benchWide   `json:"not"`
	And            []*benchWide `json:"and"`
	Or             []*benchWide `json:"or"`
	Col0           *string      `json:"col0,omitempty"`
	Col0NEQ        *string      `json:"col0NEQ,omitempty"`
	Col0In         []string     `json:"col0In,omitempty"`
	Col0NotIn      []string     `json:"col0NotIn,omitempty"`
	Col0Contains   *string      `json:"col0Contains,omitempty"`
	Col0HasPrefix  *string      `json:"col0HasPrefix,omitempty"`
	Col0IsNil      *bool        `json:"col0IsNil,omitempty"`
	Col0NotNil     *bool        `json:"col0NotNil,omitempty"`
	Col1           *int         `json:"col1,omitempty"`
	Col1NEQ        *int         `json:"col1NEQ,omitempty"`
	Col1In         []int        `json:"col1In,omitempty"`
	Col1NotIn      []int        `json:"col1NotIn,omitempty"`
	Col1GT         *int         `json:"col1GT,omitempty"`
	Col1GTE        *int         `json:"col1GTE,omitempty"`
	Col1LT         *int         `json:"col1LT,omitempty"`
	Col1LTE        *int         `json:"col1LTE,omitempty"`
	Col2           *string      `json:"col2,omitempty"`
	Col2NEQ        *string      `json:"col2NEQ,omitempty"`
	Col2In         []string     `json:"col2In,omitempty"`
	Col2NotIn      []string     `json:"col2NotIn,omitempty"`
	Col2Contains   *string      `json:"col2Contains,omitempty"`
	Col2HasPrefix  *string      `json:"col2HasPrefix,omitempty"`
	Col2IsNil      *bool        `json:"col2IsNil,omitempty"`
	Col2NotNil     *bool        `json:"col2NotNil,omitempty"`
	Col3           *int         `json:"col3,omitempty"`
	Col3NEQ        *int         `json:"col3NEQ,omitempty"`
	Col3In         []int        `json:"col3In,omitempty"`
	Col3NotIn      []int        `json:"col3NotIn,omitempty"`
	Col3GT         *int         `json:"col3GT,omitempty"`
	Col3GTE        *int         `json:"col3GTE,omitempty"`
	Col3LT         *int         `json:"col3LT,omitempty"`
	Col3LTE        *int         `json:"col3LTE,omitempty"`
	Col4           *string      `json:"col4,omitempty"`
	Col4NEQ        *string      `json:"col4NEQ,omitempty"`
	Col4In         []string     `json:"col4In,omitempty"`
	Col4NotIn      []string     `json:"col4NotIn,omitempty"`
	Col4Contains   *string      `json:"col4Contains,omitempty"`
	Col4HasPrefix  *string      `json:"col4HasPrefix,omitempty"`
	Col4IsNil      *bool        `json:"col4IsNil,omitempty"`
	Col4NotNil     *bool        `json:"col4NotNil,omitempty"`
	Col5           *int         `json:"col5,omitempty"`
	Col5NEQ        *int         `json:"col5NEQ,omitempty"`
	Col5In         []int        `json:"col5In,omitempty"`
	Col5NotIn      []int        `json:"col5NotIn,omitempty"`
	Col5GT         *int         `json:"col5GT,omitempty"`
	Col5GTE        *int         `json:"col5GTE,omitempty"`
	Col5LT         *int         `json:"col5LT,omitempty"`
	Col5LTE        *int         `json:"col5LTE,omitempty"`
	Col6           *string      `json:"col6,omitempty"`
	Col6NEQ        *string      `json:"col6NEQ,omitempty"`
	Col6In         []string     `json:"col6In,omitempty"`
	Col6NotIn      []string     `json:"col6NotIn,omitempty"`
	Col6Contains   *string      `json:"col6Contains,omitempty"`
	Col6HasPrefix  *string      `json:"col6HasPrefix,omitempty"`
	Col6IsNil      *bool        `json:"col6IsNil,omitempty"`
	Col6NotNil     *bool        `json:"col6NotNil,omitempty"`
	Col7           *int         `json:"col7,omitempty"`
	Col7NEQ        *int         `json:"col7NEQ,omitempty"`
	Col7In         []int        `json:"col7In,omitempty"`
	Col7NotIn      []int        `json:"col7NotIn,omitempty"`
	Col7GT         *int         `json:"col7GT,omitempty"`
	Col7GTE        *int         `json:"col7GTE,omitempty"`
	Col7LT         *int         `json:"col7LT,omitempty"`
	Col7LTE        *int         `json:"col7LTE,omitempty"`
	Col8           *string      `json:"col8,omitempty"`
	Col8NEQ        *string      `json:"col8NEQ,omitempty"`
	Col8In         []string     `json:"col8In,omitempty"`
	Col8NotIn      []string     `json:"col8NotIn,omitempty"`
	Col8Contains   *string      `json:"col8Contains,omitempty"`
	Col8HasPrefix  *string      `json:"col8HasPrefix,omitempty"`
	Col8IsNil      *bool        `json:"col8IsNil,omitempty"`
	Col8NotNil     *bool        `json:"col8NotNil,omitempty"`
	Col9           *int         `json:"col9,omitempty"`
	Col9NEQ        *int         `json:"col9NEQ,omitempty"`
	Col9In         []int        `json:"col9In,omitempty"`
	Col9NotIn      []int        `json:"col9NotIn,omitempty"`
	Col9GT         *int         `json:"col9GT,omitempty"`
	Col9GTE        *int         `json:"col9GTE,omitempty"`
	Col9LT         *int         `json:"col9LT,omitempty"`
	Col9LTE        *int         `json:"col9LTE,omitempty"`
	Col10          *string      `json:"col10,omitempty"`
	Col10NEQ       *string      `json:"col10NEQ,omitempty"`
	Col10In        []string     `json:"col10In,omitempty"`
	Col10NotIn     []string     `json:"col10NotIn,omitempty"`
	Col10Contains  *string      `json:"col10Contains,omitempty"`
	Col10HasPrefix *string      `json:"col10HasPrefix,omitempty"`
	Col10IsNil     *bool        `json:"col10IsNil,omitempty"`
	Col10NotNil    *bool        `json:"col10NotNil,omitempty"`
	Col11          *int         `json:"col11,omitempty"`
	Col11NEQ       *int         `json:"col11NEQ,omitempty"`
	Col11In        []int        `json:"col11In,omitempty"`
	Col11NotIn     []int        `json:"col11NotIn,omitempty"`
	Col11GT        *int         `json:"col11GT,omitempty"`
	Col11GTE       *int         `json:"col11GTE,omitempty"`
	Col11LT        *int         `json:"col11LT,omitempty"`
	Col11LTE       *int         `json:"col11LTE,omitempty"`
	Col12          *string      `json:"col12,omitempty"`
	Col12NEQ       *string      `json:"col12NEQ,omitempty"`
	Col12In        []string     `json:"col12In,omitempty"`
	Col12NotIn     []string     `json:"col12NotIn,omitempty"`
	Col12Contains  *string      `json:"col12Contains,omitempty"`
	Col12HasPrefix *string      `json:"col12HasPrefix,omitempty"`
	Col12IsNil     *bool        `json:"col12IsNil,omitempty"`
	Col12NotNil    *bool        `json:"col12NotNil,omitempty"`
	Col13          *int         `json:"col13,omitempty"`
	Col13NEQ       *int         `json:"col13NEQ,omitempty"`
	Col13In        []int        `json:"col13In,omitempty"`
	Col13NotIn     []int        `json:"col13NotIn,omitempty"`
	Col13GT        *int         `json:"col13GT,omitempty"`
	Col13GTE       *int         `json:"col13GTE,omitempty"`
	Col13LT        *int         `json:"col13LT,omitempty"`
	Col13LTE       *int         `json:"col13LTE,omitempty"`
	Col14          *string      `json:"col14,omitempty"`
	Col14NEQ       *string      `json:"col14NEQ,omitempty"`
	Col14In        []string     `json:"col14In,omitempty"`
	Col14NotIn     []string     `json:"col14NotIn,omitempty"`
	Col14Contains  *string      `json:"col14Contains,omitempty"`
	Col14HasPrefix *string      `json:"col14HasPrefix,omitempty"`
	Col14IsNil     *bool        `json:"col14IsNil,omitempty"`
	Col14NotNil    *bool        `json:"col14NotNil,omitempty"`
}

const benchWhereSDL = `
input Where {
  not: Where
  and: [Where!]
  or: [Where!]
  name: String
  nameNEQ: String
  nameIn: [String!]
  nameNotIn: [String!]
  nameContains: String
}
input Wide {
  not: Wide
  and: [Wide!]
  or: [Wide!]
  col0: String
  col0NEQ: String
  col0In: [String!]
  col0NotIn: [String!]
  col0Contains: String
  col0HasPrefix: String
  col0IsNil: Boolean
  col0NotNil: Boolean
  col1: Int
  col1NEQ: Int
  col1In: [Int!]
  col1NotIn: [Int!]
  col1GT: Int
  col1GTE: Int
  col1LT: Int
  col1LTE: Int
  col2: String
  col2NEQ: String
  col2In: [String!]
  col2NotIn: [String!]
  col2Contains: String
  col2HasPrefix: String
  col2IsNil: Boolean
  col2NotNil: Boolean
  col3: Int
  col3NEQ: Int
  col3In: [Int!]
  col3NotIn: [Int!]
  col3GT: Int
  col3GTE: Int
  col3LT: Int
  col3LTE: Int
  col4: String
  col4NEQ: String
  col4In: [String!]
  col4NotIn: [String!]
  col4Contains: String
  col4HasPrefix: String
  col4IsNil: Boolean
  col4NotNil: Boolean
  col5: Int
  col5NEQ: Int
  col5In: [Int!]
  col5NotIn: [Int!]
  col5GT: Int
  col5GTE: Int
  col5LT: Int
  col5LTE: Int
  col6: String
  col6NEQ: String
  col6In: [String!]
  col6NotIn: [String!]
  col6Contains: String
  col6HasPrefix: String
  col6IsNil: Boolean
  col6NotNil: Boolean
  col7: Int
  col7NEQ: Int
  col7In: [Int!]
  col7NotIn: [Int!]
  col7GT: Int
  col7GTE: Int
  col7LT: Int
  col7LTE: Int
  col8: String
  col8NEQ: String
  col8In: [String!]
  col8NotIn: [String!]
  col8Contains: String
  col8HasPrefix: String
  col8IsNil: Boolean
  col8NotNil: Boolean
  col9: Int
  col9NEQ: Int
  col9In: [Int!]
  col9NotIn: [Int!]
  col9GT: Int
  col9GTE: Int
  col9LT: Int
  col9LTE: Int
  col10: String
  col10NEQ: String
  col10In: [String!]
  col10NotIn: [String!]
  col10Contains: String
  col10HasPrefix: String
  col10IsNil: Boolean
  col10NotNil: Boolean
  col11: Int
  col11NEQ: Int
  col11In: [Int!]
  col11NotIn: [Int!]
  col11GT: Int
  col11GTE: Int
  col11LT: Int
  col11LTE: Int
  col12: String
  col12NEQ: String
  col12In: [String!]
  col12NotIn: [String!]
  col12Contains: String
  col12HasPrefix: String
  col12IsNil: Boolean
  col12NotNil: Boolean
  col13: Int
  col13NEQ: Int
  col13In: [Int!]
  col13NotIn: [Int!]
  col13GT: Int
  col13GTE: Int
  col13LT: Int
  col13LTE: Int
  col14: String
  col14NEQ: String
  col14In: [String!]
  col14NotIn: [String!]
  col14Contains: String
  col14HasPrefix: String
  col14IsNil: Boolean
  col14NotNil: Boolean
}
type Query { where(w: Where): String! wide(w: Wide): String! }
`

type benchWhereArgs struct {
	W *benchWhere `graphql:"w"`
}

type benchWideArgs struct {
	W *benchWide `graphql:"w"`
}

func benchInputExec(t testing.TB, seen func(any)) *Executor {
	s, err := NewSchema(SDL(benchWhereSDL),
		Input[benchWhere]("Where", ZeroForNull()),
		Input[benchWide]("Wide", ZeroForNull()),
		Args[benchWhereArgs](), Args[benchWideArgs](),
		Query(
			ResolveArgs("where", func(_ context.Context, _ Root, a benchWhereArgs) (string, error) {
				seen(a.W)
				return "ok", nil
			}),
			ResolveArgs("wide", func(_ context.Context, _ Root, a benchWideArgs) (string, error) {
				seen(a.W)
				return "ok", nil
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewExecutor(s)
}

var benchInputRequests = map[string]*Request{
	"Nested": {
		Query:     `query($w: Where) { where(w: $w) }`,
		Variables: []byte(`{"w":{"or":[{"nameContains":"a"},{"and":[{"nameIn":["x","y","z"]},{"not":{"name":"q"}}]}]}}`),
	},
	"Wide": {
		Query:     `query($w: Wide) { wide(w: $w) }`,
		Variables: []byte(`{"w":{"col0Contains":"a","col1GT":3}}`),
	},
}

func BenchmarkExecuteInputVariable(b *testing.B) {
	e := benchInputExec(b, func(any) {})
	for _, name := range []string{"Nested", "Wide"} {
		req := benchInputRequests[name]
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				resp := e.Execute(context.Background(), req)
				if len(resp.Errors) > 0 {
					b.Fatal(resp.Errors[0])
				}
				resp.Release()
			}
		})
	}
}

// The benchmark is only worth reading if the value it times arrives whole.
func TestBenchInputDecodes(t *testing.T) {
	var got any
	e := benchInputExec(t, func(v any) { got = v })
	run(t, e, benchInputRequests["Nested"].Query, string(benchInputRequests["Nested"].Variables))
	w := got.(*benchWhere)
	if len(w.Or) != 2 || *w.Or[0].NameContains != "a" || len(w.Or[1].And) != 2 ||
		len(w.Or[1].And[0].NameIn) != 3 || *w.Or[1].And[1].Not.Name != "q" {
		t.Fatalf("nested decoded as %+v", w)
	}
	run(t, e, benchInputRequests["Wide"].Query, string(benchInputRequests["Wide"].Variables))
	wd := got.(*benchWide)
	if *wd.Col0Contains != "a" || *wd.Col1GT != 3 || wd.Col2 != nil {
		t.Fatalf("wide decoded as %+v", wd)
	}
}
