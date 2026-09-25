package graphql

import (
	"context"
	"testing"
)

// A nil Go slice is the zero value of a slice and the idiomatic empty list:
// len 0, range and append all work, and an ORM query with no rows returns
// one. At a non-null list position null is never a valid answer, so a nil
// slice there is written as []. At a nullable position it stays null, so nil
// and []T{} still say "no list" and "an empty one".
//
// Measured before the change, same schema, same input:
//
//	                          nil / null in [T!]!         [] in [T!]!
//	graphql-js 17.0.2         error (JS has no nil slice)  []
//	gqlgen v0.17.95           []                           []
//	graphql-go, before        error                        []
//
// graphql-js cannot decide it -- a JS resolver says null or [] and means it
// -- and gqlgen, which consumers migrate from, writes []. Keeping the error
// made every gqlgen resolver that returns nil for an empty non-null list fail
// at request time, where neither the compiler nor NewSchema sees it.
func TestNilSliceAtNonNullListIsEmpty(t *testing.T) {
	type item struct{ N int }
	s, err := NewSchema(SDL(`type Item { n: Int! }
type Query {
  objects: [Item!]!
  nullableObjects: [Item!]
  ints: [Int!]!
  nullableInts: [Int!]
  nested: [[Int!]!]!
  nestedObjects: [[Item!]!]!
}`),
		Object[item]("Item", Field("n", func(v *item) int { return v.N })),
		Query(
			Resolve("objects", func(context.Context, Root) ([]*item, error) { return nil, nil }),
			Resolve("nullableObjects", func(context.Context, Root) ([]*item, error) { return nil, nil }),
			Resolve("ints", func(context.Context, Root) ([]int, error) { return nil, nil }),
			Resolve("nullableInts", func(context.Context, Root) ([]int, error) { return nil, nil }),
			Resolve("nested", func(context.Context, Root) ([][]int, error) { return [][]int{nil, {1}}, nil }),
			Resolve("nestedObjects", func(context.Context, Root) ([][]*item, error) { return [][]*item{nil}, nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s)

	resp := run(t, e, `{ objects { n } nullableObjects { n } ints nullableInts nested nestedObjects { n } }`, "")
	expectData(t, resp, `{"objects":[],"nullableObjects":null,"ints":[],"nullableInts":null,"nested":[[],[1]],"nestedObjects":[[]]}`)
}
