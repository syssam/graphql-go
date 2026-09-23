package graphql

import (
	"context"
	"math"
	"testing"
)

// Three nested first: 2147483647 lists multiply past the range of int. The
// unsaturated product wrapped negative, came in under Max and executed, and
// ext/throttle then charged the negative quote as credit.
func TestQueryCostSaturatesInsteadOfWrapping(t *testing.T) {
	type node struct{ ID ID }
	type pageArgs struct{ First *int }
	s, err := NewSchema(SDL(`
		type Node { id: ID! children(first: Int): [Node!]! }
		type Query { root(first: Int): [Node!]! }
	`),
		Args[pageArgs](),
		Object[node]("Node",
			Field("id", func(n *node) ID { return n.ID }),
			ResolveArgs("children", func(context.Context, *node, pageArgs) ([]*node, error) { return nil, nil }),
		),
		Query(ResolveArgs("root", func(context.Context, Root, pageArgs) ([]*node, error) { return nil, nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, WithQueryCost(QueryCost{Max: 1000}))
	for _, q := range []string{
		`{ root(first: 2147483647) { children(first: 2147483647) { children(first: 2147483647) { id } } } }`,
		`{ root(first: 2147483647) { children(first: 2147483647) { children(first: 2147483647) { children(first: 2147483647) { id } } } } }`,
	} {
		resp := run(t, e, q, "")
		if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeTooComplex {
			t.Fatalf("%s: expected a cost rejection, got data %s errors %s", q, resp.Data, errorsJSON(resp.Errors))
		}
		if got := resp.Errors[0].Extensions["requestedQueryCost"]; got != math.MaxInt {
			t.Fatalf("%s: requestedQueryCost = %v, want the saturated %d", q, got, math.MaxInt)
		}
	}
}

func TestCostArithmeticSaturates(t *testing.T) {
	for _, c := range []struct{ got, want int }{
		{costAdd(math.MaxInt, 1), math.MaxInt},
		{costAdd(math.MaxInt-1, 1), math.MaxInt},
		{costAdd(math.MinInt, -1), math.MinInt},
		{costAdd(2, 3), 5},
		{costAdd(-2, 3), 1},
		{costMul(math.MaxInt/2+1, 2), math.MaxInt},
		{costMul(math.MinInt/2-1, 2), math.MinInt},
		{costMul(7, 3), 21},
		{costMul(0, math.MaxInt), 0},
		{costMul(-7, 3), -21},
	} {
		if c.got != c.want {
			t.Errorf("got %d, want %d", c.got, c.want)
		}
	}
}
