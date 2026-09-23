package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// buildAuthShape sorts the possible-type names before descending, "so a
// document's site indices do not depend on map order". That mattered enough to
// write down and nothing checked it: deleting the sort leaves every test in
// this repository green, five runs in a row.
//
// The index is not an internal detail. Decision.Set(site int, Outcome) is how
// an Authorizer answers, so the order of AuthShape.Sites() *is* the contract
// between the two. A plan is compiled once, so a single request is consistent
// whatever the order — but the plan cache is a bounded LRU, so the same
// document recompiles after eviction, and an Authorizer that memoized "site 3
// is allow for this operation" would then apply that to whatever site 3 became.
// Silent, intermittent and only under cache pressure.
//
// One interface, several implementers, each declaring a requirement: byType is
// a map, so this is the walk the sort exists for.
const shapeOrderSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT

interface Node { id: ID! }

type Alpha implements Node { id: ID! a: String! @requiresScopes(scopes: [["a:read"]]) }
type Bravo implements Node { id: ID! b: String! @requiresScopes(scopes: [["b:read"]]) }
type Delta implements Node { id: ID! d: String! @requiresScopes(scopes: [["d:read"]]) }
type Echo  implements Node { id: ID! e: String! @requiresScopes(scopes: [["e:read"]]) }
type Gamma implements Node { id: ID! g: String! @requiresScopes(scopes: [["g:read"]]) }

type Query { node: Node }
`

type shapeOrderNode struct{ ID ID }

func shapeOrderSchema(t *testing.T) *Schema {
	t.Helper()
	opts := []SchemaOption{
		Interface[any]("Node"),
		Query(Resolve("node", func(context.Context, Root) (any, error) { return nil, nil })),
	}
	for name, field := range map[string]string{
		"Alpha": "a", "Bravo": "b", "Delta": "d", "Echo": "e", "Gamma": "g",
	} {
		opts = append(opts, Object[shapeOrderNode](name,
			Field("id", func(n *shapeOrderNode) ID { return n.ID }),
			Field(field, func(*shapeOrderNode) string { return "" }),
		))
	}
	s, err := NewSchema(SDL(shapeOrderSDL), opts...)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}

const shapeOrderQuery = `{ node {
	id
	... on Alpha { a }
	... on Bravo { b }
	... on Delta { d }
	... on Echo  { e }
	... on Gamma { g }
} }`

// sitesOf compiles the document in a fresh executor — so a fresh plan, as an
// eviction would produce — and reports the site coordinates in index order.
func sitesOf(t *testing.T, s *Schema) string {
	t.Helper()
	var coords []string
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(_ context.Context, shape *AuthShape, _ *Decision) error {
			for _, site := range shape.Sites() {
				coords = append(coords, site.Coord)
			}
			return nil
		})))
	resp := run(t, e, shapeOrderQuery, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if len(coords) == 0 {
		t.Fatal("the authorizer saw no sites")
	}
	return strings.Join(coords, ",")
}

func TestAuthSiteOrderDoesNotDependOnMapOrder(t *testing.T) {
	s := shapeOrderSchema(t)
	want := sitesOf(t, s)
	// Enough compiles that Go's randomized map iteration would have shown a
	// different order with overwhelming probability: five implementers means
	// 120 orderings, and only one of them is this one.
	for i := range 40 {
		if got := sitesOf(t, s); got != want {
			t.Fatalf("compile %d produced a different site order\n got: %s\nwant: %s\n"+
				"Decision.Set keys by index, so an Authorizer that memoizes a decision "+
				"for this document applies it to a different site after the plan cache "+
				"evicts and recompiles", i, got, want)
		}
	}
	t.Logf("%d compiles, identical order: %s", 41, want)
}

// And the same document must produce the same order across two schemas built
// from the same SDL, since a process restart is the other way a plan is
// compiled afresh.
func TestAuthSiteOrderIsTheSameAcrossSchemaBuilds(t *testing.T) {
	first := sitesOf(t, shapeOrderSchema(t))
	for i := range 10 {
		if got := sitesOf(t, shapeOrderSchema(t)); got != first {
			t.Fatalf("schema build %d produced a different site order\n got: %s\nwant: %s",
				i, got, first)
		}
	}
	// The order is the document's, so it is the SDL's declaration order of the
	// possible types sorted by name, not the order the fragments appear in.
	if !strings.Contains(first, fmt.Sprintf("%s.a", "Alpha")) {
		t.Errorf("sites do not name the expected coordinates: %s", first)
	}
}
