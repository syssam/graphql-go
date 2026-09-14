package compare_test

import (
	"encoding/json"
	"testing"

	"github.com/syssam/graphql-go/compare"
)

// queries are run through both engines. They walk the paths that differ
// between the two: leaf fields, a resolver-backed object, a Relay connection
// with edges and page info, two levels of nesting, and a mutation.
var queries = map[string]string{
	"leaf fields": `{ entity000(id: "3") { id name description active score ratio tags } }`,

	"owner": `{ entity000(id: "3") { id owner { id name } } }`,

	"connection": `{ entity000s(first: 5) {
		totalCount
		pageInfo { hasNextPage startCursor endCursor }
		edges { cursor node { id name } }
	} }`,

	"nested connection": `{ entity000s(first: 3) {
		edges { node { id children(first: 2) { totalCount edges { node { id name } } } } }
	} }`,

	"owner chain": `{ entity000s(first: 2) {
		edges { node { id owner { id owner { id name } } } }
	} }`,

	"mutation": `mutation { updateEntity000(id: "1", input: { name: "renamed" }) { id name } }`,
}

// normalise re-encodes a response so key order cannot cause a false mismatch;
// Go sorts map keys when marshalling.
func normalise(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

// TestEnginesAgree is the claim this module exists to support: given one
// schema, one set of structs and one dataset, both engines return the same
// bytes. Without it, any benchmark could be comparing different work.
func TestEnginesAgree(t *testing.T) {
	ours, err := compare.NewGraphQLGo()
	if err != nil {
		t.Fatalf("graphql-go schema: %v", err)
	}
	theirs := compare.NewGqlgen()

	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			a, err := compare.GraphQLGoJSON(ours, q)
			if err != nil {
				t.Fatalf("graphql-go: %v", err)
			}
			b, err := compare.GqlgenJSON(theirs, q)
			if err != nil {
				t.Fatalf("gqlgen: %v", err)
			}
			if got, want := normalise(t, a), normalise(t, b); got != want {
				t.Errorf("engines disagree\ngraphql-go: %s\ngqlgen:     %s", got, want)
			}
		})
	}
}
