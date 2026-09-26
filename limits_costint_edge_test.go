package graphql

import (
	"encoding/json"
	"math"
	"testing"
)

// Every Go type and spelling a page size can arrive as, including the ones
// whose conversion to int is implementation defined unless clamped first.
func TestAsCostIntEdges(t *testing.T) {
	cases := []struct {
		in   any
		want int
		ok   bool
	}{
		{int(7), 7, true},
		{int32(7), 7, true},
		{int64(7), 7, true},
		{json.Number("7"), 7, true},
		{json.Number("5e2"), 500, true},
		{json.Number("500.0"), 500, true},
		{json.Number("1e999"), math.MaxInt, true},
		{json.Number("-1e999"), math.MinInt, true},
		{json.Number("abc"), 0, false},
		{float64(2.9), 2, true},
		{float64(1e300), math.MaxInt, true},
		{float64(-1e300), math.MinInt, true},
		{math.Inf(1), math.MaxInt, true},
		{math.NaN(), 0, false},
		{"7", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := asCostInt(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("asCostInt(%#v) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
