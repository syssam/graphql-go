package fed_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

// Representation.ID, Int and Float are the whole reading API of a federated
// entity resolver, and they exist because the obvious assertion is wrong: a
// number in a representation is a json.Number, so r["id"].(float64) compiles,
// looks right and silently yields zero. Nothing exercised them -- the other
// tests in this package read r["id"].(string) by hand, which is the one shape
// that happens not to need them.

// numEntity has a numeric @key, which is what makes the accessors load-bearing
// rather than a convenience over a string.
type numEntity struct {
	ID    int64
	Label string
}

const numSDL = `
extend schema @link(url: "https://specs.apollo.dev/federation/v2.3", import: ["@key"])

type Item @key(fields: "id") {
  id: ID!
  label: String!
}

type Query { anItem: Item! }
`

// TestRepresentationAccessorsReadWhatTheDecoderProduces drives a real
// _entities request, so the claim in Representation's doc -- that a number
// arrives as json.Number and not as float64 -- is checked against the decoder
// rather than against a hand-built map. If that ever changed, ID would return
// ("", false) for every numeric key and every federated lookup would fail
// with no error anywhere.
func TestRepresentationAccessorsReadWhatTheDecoderProduces(t *testing.T) {
	var seen fed.Representation
	src, opt, err := fed.Subgraph(numSDL,
		fed.Resolver("Item", func(_ context.Context, r fed.Representation) (*numEntity, error) {
			seen = r
			id, ok := r.ID("id")
			if !ok {
				return nil, nil
			}
			n, ok := r.Int("id")
			if !ok {
				return nil, nil
			}
			return &numEntity{ID: n, Label: string(id)}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	s, err := graphql.NewSchema(src, opt,
		graphql.Object[numEntity]("Item",
			graphql.Field("id", func(e *numEntity) graphql.ID { return graphql.ID(e.Label) }),
			graphql.Field("label", func(e *numEntity) string { return e.Label }),
		),
		graphql.Query(graphql.Field("anItem", func(graphql.Root) *numEntity { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp := graphql.NewExecutor(s).Execute(context.Background(), &graphql.Request{
		Query: `query($r:[_Any!]!){ _entities(representations:$r){ ... on Item { id label } } }`,
		Variables: json.RawMessage(
			`{"r":[{"__typename":"Item","id":7}]}`),
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"_entities":[{"id":"7","label":"7"}]}` {
		t.Fatalf("data = %s; the accessors did not read the numeric key", got)
	}

	// And the trap the accessors exist to close is still a trap, which is why
	// they are not optional sugar.
	if f, ok := seen["id"].(float64); ok {
		t.Errorf("id asserted to float64 and succeeded with %v; Representation's doc says it cannot", f)
	}
	if _, ok := seen["id"].(json.Number); !ok {
		t.Errorf("id is %T, not json.Number; ID and Int would both return false for it", seen["id"])
	}
}

// The accessors also take the Go-native forms, because a batch resolver or a
// test may build a Representation by hand rather than decode one.
func TestRepresentationAccessorsAcceptEveryForm(t *testing.T) {
	r := fed.Representation{
		"__typename": "Item",
		"str":        "abc",
		"num":        json.Number("42"),
		"frac":       json.Number("1.5"),
		"i64":        int64(7),
		"f64":        float64(2.5),
		"whole":      float64(9),
		"boolean":    true,
	}
	if got := r.Typename(); got != "Item" {
		t.Errorf("Typename = %q", got)
	}
	for _, c := range []struct {
		key  string
		want graphql.ID
		ok   bool
	}{
		{"str", "abc", true},
		{"num", "42", true},
		{"boolean", "", false},
		{"missing", "", false},
	} {
		if got, ok := r.ID(c.key); got != c.want || ok != c.ok {
			t.Errorf("ID(%q) = %q,%v want %q,%v", c.key, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		key  string
		want int64
		ok   bool
	}{
		{"num", 42, true},
		{"i64", 7, true},
		{"whole", 9, true},
		// A float that is not whole is not an integer, and rounding it would
		// answer a key lookup with the wrong row.
		{"f64", 0, false},
		{"frac", 0, false},
		{"str", 0, false},
	} {
		if got, ok := r.Int(c.key); got != c.want || ok != c.ok {
			t.Errorf("Int(%q) = %d,%v want %d,%v", c.key, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		key  string
		want float64
		ok   bool
	}{
		{"frac", 1.5, true},
		{"f64", 2.5, true},
		{"i64", 7, true},
		{"num", 42, true},
		{"str", 0, false},
	} {
		if got, ok := r.Float(c.key); got != c.want || ok != c.ok {
			t.Errorf("Float(%q) = %v,%v want %v,%v", c.key, got, ok, c.want, c.ok)
		}
	}
}

// _Any decodes whatever the router sent, and a router is not the only thing
// that can reach this endpoint: representations arrive as a variable, so a
// scalar or a list where an object belongs is one request away. The decoder
// has to refuse it as a request error rather than panic or resolve an empty
// entity, and nothing drove that branch.
func TestEntitiesRefusesARepresentationThatIsNotAnObject(t *testing.T) {
	for _, bad := range []string{`"nope"`, `42`, `[]`, `true`} {
		t.Run(bad, func(t *testing.T) {
			resp := entities(t, newSubgraph(t), `[`+bad+`]`, `... on User { id }`)
			if len(resp.Errors) == 0 {
				t.Fatalf("representation %s was accepted; data = %s", bad, resp.Data)
			}
			if got := resp.Errors[0].Message; !strings.Contains(got, "_Any") {
				t.Errorf("error does not name the scalar that refused it: %q", got)
			}
		})
	}
}
