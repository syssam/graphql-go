package compare_test

import (
	"testing"

	"github.com/syssam/graphql-go/compare"
)

// benchQueries are the shapes worth timing. Each one is verified by
// TestEnginesAgree to produce identical output on both engines, so the
// timings compare the same work.
var benchQueries = []struct{ name, query string }{
	{"Leaf", `{ entity000(id: "3") { id name description active score ratio tags } }`},

	{"Owner", `{ entity000(id: "3") { id owner { id name } } }`},

	{"Connection", `{ entity000s(first: 20) {
		totalCount
		pageInfo { hasNextPage startCursor endCursor }
		edges { cursor node { id name score } }
	} }`},

	{"NestedConnection", `{ entity000s(first: 20) {
		edges { node { id children(first: 5) { totalCount edges { node { id name } } } } }
	} }`},

	{"Mutation", `mutation { updateEntity000(id: "1", input: { name: "renamed" }) { id name } }`},

	{"Introspection", introspectionQuery},
}

func BenchmarkGraphQLGo(b *testing.B) {
	e, err := compare.NewGraphQLGo()
	if err != nil {
		b.Fatal(err)
	}
	for _, q := range benchQueries {
		b.Run(q.name, func(b *testing.B) {
			if _, err := compare.GraphQLGoJSON(e, q.query); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := compare.GraphQLGoJSON(e, q.query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGqlgen(b *testing.B) {
	e := compare.NewGqlgen()
	for _, q := range benchQueries {
		b.Run(q.name, func(b *testing.B) {
			if _, err := compare.GqlgenJSON(e, q.query); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := compare.GqlgenJSON(e, q.query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSchemaBuild measures start-up rather than steady state: building
// the schema is where a large type graph costs the most.
func BenchmarkSchemaBuild(b *testing.B) {
	b.Run("GraphQLGo", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := compare.NewGraphQLGo(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Gqlgen", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			compare.NewGqlgen()
		}
	})
}

// introspectionQuery is what a client tool sends on connect. Over a large
// schema it is the most expensive query either engine will serve.
const introspectionQuery = `{
  __schema {
    queryType { name }
    mutationType { name }
    types {
      kind
      name
      fields(includeDeprecated: true) {
        name
        args { name type { kind name ofType { kind name } } }
        type { kind name ofType { kind name ofType { kind name } } }
      }
      inputFields { name type { kind name ofType { kind name } } }
      interfaces { kind name }
      enumValues(includeDeprecated: true) { name }
      possibleTypes { kind name }
    }
  }
}`
