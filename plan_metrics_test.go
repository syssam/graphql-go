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
// has refused nothing. It asserts a structural bound on walkConcrete's call
// count after the walk returns, rather than the walk enforcing a cap on
// itself (see metricWalker.calls): production code has no business turning
// "this document is merely large" into an error on this test's behalf, and
// Task 5 is precisely the commit that wires this walker into real limit
// enforcement, where that call would not be this test's to make.
//
// Depth 6 is chosen, not the fanTypes^9 used elsewhere in this file, because
// an unmemoized walk at this depth still finishes in about 2.4s, measured
// directly (see task-4-report.md), instead of needing a hang-avoidance
// mechanism: 2,396,745 calls unmemoized against 57 memoized is already a
// four-order-of-magnitude gap, which is all this test needs to tell a
// working memo from a broken one.
func TestOperationMetricsIsBounded(t *testing.T) {
	s, e := newFanExecutor(t)
	const depth = 6
	entry, errs := e.document(fanQuery(depth))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	op := entry.doc.Operations[0]
	root := s.rootFor(op.Operation)

	w := &metricWalker{
		c:    &compiler{s: s, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
		memo: make(map[selKey]planMetrics),
	}
	// fanQuery(n) chains root -> next (n times) -> id, so the plan-tree
	// depth (the specification depthOf/complexityOf are checked against) is
	// n+2; verified directly against compilePlan for n=0..9 in an earlier
	// pass (task-4-report.md), where it is cheap because Task 2 already
	// memoizes plan compilation.
	m := w.walk(root, nil, op.SelectionSet)
	if m.depth != depth+2 {
		t.Fatalf("depth = %d, want %d", m.depth, depth+2)
	}
	t.Logf("walkConcrete calls = %d", w.calls)
	// want is a generous multiple of the memoized call count (one call per
	// fanTypes type per nesting level, comfortably under 100 here), but far
	// below the 2,396,745 calls an unmemoized walk needs, measured directly
	// (see task-4-report.md).
	const want = 200
	if w.calls > want {
		t.Fatalf("walkConcrete called %d times, want at most %d; the memo is not deduplicating", w.calls, want)
	}
}
