package graphql

import (
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

// metricsCases covers the constructs where an AST walk and a plan-tree walk
// can disagree: abstract parents taking a max over concrete types, duplicate
// response keys merging, fragments, and skip/include folding. vars is nil
// for cases with no variables; where set, it is decoded into a cond map via
// variantKey (the same path planFor uses), so operationMetrics and the
// compiled plan are compared under the same variant.
var metricsCases = []struct {
	name  string
	query string
	vars  map[string]any
}{
	{"flat", `{ root { id } }`, nil},
	{"nested", `{ root { id next { id next { id } } } }`, nil},
	{"duplicate keys", `{ root { next { id } next { id } } }`, nil},
	{"inline fragment", `{ root { ... on T0 { id } ... on T1 { id next { id } } } }`, nil},
	{"named fragment", `fragment F on Node { id next { id } } { root { ...F } }`, nil},
	{"typename", `{ root { __typename next { __typename id } } }`, nil},
	{"skip literal", `{ root { id @skip(if: true) next { id } } }`, nil},
	{"include literal", `{ root { id @include(if: false) next { id } } }`, nil},
	{
		"skip variable true",
		`query($s: Boolean!) { root { id @skip(if: $s) next { id } } }`,
		map[string]any{"s": true},
	},
	{
		"skip variable false",
		`query($s: Boolean!) { root { id @skip(if: $s) next { id } } }`,
		map[string]any{"s": false},
	},
}

func TestOperationMetricsMatchesPlan(t *testing.T) {
	s, e := newFanExecutor(t)
	for _, tc := range metricsCases {
		t.Run(tc.name, func(t *testing.T) {
			entry, errs := e.document(tc.query)
			if errs != nil {
				t.Fatalf("document: %v", errs[0])
			}
			op := entry.doc.Operations[0]
			p, _, perrs := entry.planFor(s, e, op, tc.vars)
			if perrs != nil {
				t.Fatalf("plan: %v", perrs[0])
			}
			// variantKey is the same decode planFor uses internally, so cond
			// here matches the variant p was actually compiled for.
			_, cond := variantKey(entry.condVars, tc.vars)
			got := operationMetrics(s, entry.doc, op, cond)
			if got.complexity != p.complexity {
				t.Errorf("complexity = %d, plan says %d", got.complexity, p.complexity)
			}
			if got.depth != p.depth {
				t.Errorf("depth = %d, plan says %d", got.depth, p.depth)
			}
		})
	}
}

// TestOperationMetricsIsBounded is the reason the walk carries its own memo:
// a guard that expands N^depth to decide whether to refuse to expand N^depth
// has refused nothing. Besides asserting the resulting depth, it asserts a
// structural bound on walkConcrete's call count, so that if the memo ever
// regresses, this test fails fast and deterministically rather than hanging:
// a bare depth assertion would never even run, because an unmemoized walk of
// fanQuery(9) would not return in any reasonable time. See metricWalker's
// budget field: it is what makes this failure fast instead of a hang.
func TestOperationMetricsIsBounded(t *testing.T) {
	s, e := newFanExecutor(t)
	entry, errs := e.document(fanQuery(9))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	op := entry.doc.Operations[0]
	root := s.rootFor(op.Operation)

	// budget is a generous multiple of the memoized call count (well under
	// 100 for this fixture: one call per fanTypes type per nesting level),
	// but far below the fanTypes^9 an unmemoized walk would need, so it
	// trips within a handful of calls instead of running to completion.
	const budget = 10_000
	w := &metricWalker{
		c:      &compiler{s: s, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
		memo:   make(map[selKey]planMetrics),
		budget: budget,
	}
	// Depth 9 is fanTypes^9 unfolded; if this returns at all, the walk is
	// memoized. The plan is deliberately never compiled here.
	//
	// fanQuery(n) chains root -> next (n times) -> id, so the plan-tree
	// depth (the specification depthOf/complexityOf are checked against) is
	// n+2; verified directly against compilePlan for n=0..9, where it is
	// still cheap because Task 2 already memoizes plan compilation.
	m := w.walk(root, nil, op.SelectionSet)
	if m.depth != 11 {
		t.Fatalf("depth = %d, want 11", m.depth)
	}
	t.Logf("walkConcrete calls = %d", w.calls)
	if w.calls > budget {
		t.Fatalf("walkConcrete called %d times, want at most %d; the memo is not deduplicating", w.calls, budget)
	}
}
