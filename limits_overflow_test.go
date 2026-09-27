package graphql

import (
	"context"
	"fmt"
	"math"
	"strings"
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

// Each fragment selects bestFriend twice under two aliases and spreads the
// next, so the tree doubles at every level while the document grows by one
// line. The metric walk is memoized over that DAG, but it summed tree size
// with plain addition: at 64 levels the sum wrapped to -1, came in under
// WithMaxComplexity, and a 5 KB request held gigabytes before anything
// stopped it. Complexity saturates as the cost does.
func TestComplexitySaturatesInsteadOfWrapping(t *testing.T) {
	// The response cap only keeps a regression from exhausting the runner's
	// memory; the rejection must come from the complexity limit.
	_, e := newFixtureExecutor(t, WithMaxComplexity(1000), WithMaxResponseBytes(1<<16))
	for _, levels := range []int{63, 64, 65, 128} {
		var b strings.Builder
		b.WriteString("{ me { ...F0 } }\n")
		for i := range levels {
			fmt.Fprintf(&b, "fragment F%d on User { a: bestFriend { ...F%d } b: bestFriend { ...F%d } }\n", i, i+1, i+1)
		}
		fmt.Fprintf(&b, "fragment F%d on User { id }\n", levels)
		resp := run(t, e, b.String(), "")
		if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeTooComplex {
			t.Fatalf("%d levels: expected a complexity rejection, got %d bytes of data, errors %s", levels, len(resp.Data), errorsJSON(resp.Errors))
		}
		if got := resp.Errors[0].Extensions["complexity"]; got != math.MaxInt {
			t.Fatalf("%d levels: complexity = %v, want the saturated %d", levels, got, math.MaxInt)
		}
	}
}
