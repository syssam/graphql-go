package graphql

import (
	"math"
	"testing"
)

// The actual cost is the one sum over a request that did not saturate: it is
// 32 bits to fit execState's padding, and three fields weighing 2^30 wrapped
// it negative, which ext/throttle then refunded.
func TestActualCostSaturatesInsteadOfWrapping(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int! }`), Query(Field("a", func(Root) int { return 1 })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	for name, weight := range map[string]int{"sums past int32": 1 << 30, "one weight past int32": math.MaxInt32 + 5} {
		t.Run(name, func(t *testing.T) {
			e := NewExecutor(s, WithQueryCost(QueryCost{Report: true, Actual: true, FieldWeight: map[string]int{"Query.a": weight}}))
			resp := run(t, e, `{ a x: a y: a }`, "")
			cost, _ := resp.Extensions["cost"].(map[string]any)
			got, ok := cost["actualQueryCost"].(int)
			if !ok {
				t.Fatalf("extensions.cost = %v, want an integer actualQueryCost", resp.Extensions["cost"])
			}
			if got != math.MaxInt32 {
				t.Fatalf("actualQueryCost = %d, want it held at %d", got, math.MaxInt32)
			}
		})
	}
}

// A weight may be negative -- a field an operator discounts -- so a negative
// running total is not by itself a wrapped one. The first version of the
// saturation read it as one and replaced a total of -4 with MaxInt32.
func TestActualCostKeepsALegitimatelyNegativeTotal(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { cached: Int! name: Int! }`), Query(
		Field("cached", func(Root) int { return 1 }),
		Field("name", func(Root) int { return 1 }),
	))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithQueryCost(QueryCost{Report: true, Actual: true,
		FieldWeight: map[string]int{"Query.cached": -5, "Query.name": 1}}))
	resp := run(t, e, `{ cached name }`, "")
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if got := cost["actualQueryCost"]; got != -4 {
		t.Fatalf("actualQueryCost = %v, want -4", got)
	}
}

// And it saturates downwards as well as up.
func TestActualCostSaturatesAtTheNegativeBoundToo(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { a: Int! }`), Query(Field("a", func(Root) int { return 1 })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithQueryCost(QueryCost{Report: true, Actual: true, FieldWeight: map[string]int{"Query.a": -(1 << 30)}}))
	resp := run(t, e, `{ a x: a y: a }`, "")
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if got := cost["actualQueryCost"]; got != math.MinInt32 {
		t.Fatalf("actualQueryCost = %v, want it held at %d", got, math.MinInt32)
	}
}
