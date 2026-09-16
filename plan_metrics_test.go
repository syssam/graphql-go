package graphql

import "testing"

// metricsCases covers the constructs where an AST walk and a plan-tree walk
// can disagree: abstract parents taking a max over concrete types, duplicate
// response keys merging, fragments, and skip/include folding.
var metricsCases = []struct {
	name  string
	query string
}{
	{"flat", `{ root { id } }`},
	{"nested", `{ root { id next { id next { id } } } }`},
	{"duplicate keys", `{ root { next { id } next { id } } }`},
	{"inline fragment", `{ root { ... on T0 { id } ... on T1 { id next { id } } } }`},
	{"named fragment", `fragment F on Node { id next { id } } { root { ...F } }`},
	{"typename", `{ root { __typename next { __typename id } } }`},
	{"skip literal", `{ root { id @skip(if: true) next { id } } }`},
	{"include literal", `{ root { id @include(if: false) next { id } } }`},
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
			p, _, perrs := entry.planFor(s, e, op, nil)
			if perrs != nil {
				t.Fatalf("plan: %v", perrs[0])
			}
			got := operationMetrics(s, entry.doc, op, nil)
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
// has refused nothing.
func TestOperationMetricsIsBounded(t *testing.T) {
	s, e := newFanExecutor(t)
	entry, errs := e.document(fanQuery(9))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	// Depth 9 is fanTypes^9 unfolded; if this returns at all, the walk is
	// memoized. The plan is deliberately never compiled here.
	//
	// fanQuery(n) chains root -> next (n times) -> id, so the plan-tree
	// depth (the specification depthOf/complexityOf are checked against) is
	// n+2; verified directly against compilePlan for n=0..9, where it is
	// still cheap because Task 2 already memoizes plan compilation.
	if m := operationMetrics(s, entry.doc, entry.doc.Operations[0], nil); m.depth != 11 {
		t.Fatalf("depth = %d, want 11", m.depth)
	}
}
