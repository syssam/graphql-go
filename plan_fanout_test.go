package graphql

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fanTagN give fanT distinct instantiations. The registry keys bindings by
// (GraphQL type name, reflect.Type), so the eight schema types need eight Go
// types; generic instantiation supplies them without eight declarations.
type (
	fanTag0 struct{}
	fanTag1 struct{}
	fanTag2 struct{}
	fanTag3 struct{}
	fanTag4 struct{}
	fanTag5 struct{}
	fanTag6 struct{}
	fanTag7 struct{}
)

const fanTypes = 8

type fanT[T any] struct{ ID string }

func (t *fanT[T]) fanNodeID() string { return t.ID }

type fanNode interface{ fanNodeID() string }

// fanSDL is cyclic through the abstract type: Node.next returns Node, so a
// planner that expands every possible type at every level does fanTypes^depth
// work for a query that only ever resolves one concrete type per level.
func fanSDL() string {
	var b strings.Builder
	b.WriteString("interface Node { id: ID! next: Node }\n")
	for i := 0; i < fanTypes; i++ {
		fmt.Fprintf(&b, "type T%d implements Node { id: ID! next: Node }\n", i)
	}
	b.WriteString("type Query { root: Node }\n")
	return b.String()
}

func fanObject[T any](name string) SchemaOption {
	return Object[fanT[T]](name,
		Field("id", func(t *fanT[T]) string { return t.ID }),
		Field("next", func(t *fanT[T]) fanNode { return nil }),
	)
}

func newFanExecutor(t *testing.T, opts ...ExecutorOption) (*Schema, *Executor) {
	t.Helper()
	s, err := NewSchema(SDL(fanSDL()),
		fanObject[fanTag0]("T0"),
		fanObject[fanTag1]("T1"),
		fanObject[fanTag2]("T2"),
		fanObject[fanTag3]("T3"),
		fanObject[fanTag4]("T4"),
		fanObject[fanTag5]("T5"),
		fanObject[fanTag6]("T6"),
		fanObject[fanTag7]("T7"),
		Object[Root]("Query", Field("root", func(Root) fanNode { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s, NewExecutor(s, opts...)
}

// fanQuery nests `next` depth levels below root.
func fanQuery(depth int) string {
	var b strings.Builder
	b.WriteString("{ root { id ")
	for i := 0; i < depth; i++ {
		b.WriteString("next { id ")
	}
	b.WriteString(strings.Repeat("} ", depth))
	b.WriteString("} }")
	return b.String()
}

func fanPlan(t *testing.T, s *Schema, e *Executor, depth int) *plan {
	t.Helper()
	entry, errs := e.document(fanQuery(depth))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	p, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs[0])
	}
	return p
}

// countSelectionSets counts selection sets by pointer identity, so a memoized
// compile that shares one set across many paths counts it once.
func countSelectionSets(sel *selectionSet) int {
	seen := make(map[*selectionSet]bool)
	var walk func(*selectionSet)
	walk = func(s *selectionSet) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		for _, sub := range s.byType {
			walk(sub)
		}
		for _, f := range s.fields {
			walk(f.sub)
		}
	}
	walk(sel)
	return len(seen)
}

// TestFanOutExpansionIsBounded is the regression test for the plan blowup.
// Counting selection sets rather than timing the compile keeps it
// deterministic: the bound is a property of the algorithm, not of the machine.
//
// Before memoization this compiles fanTypes^6 = 262,144 selection sets.
func TestFanOutExpansionIsBounded(t *testing.T) {
	s, e := newFanExecutor(t)
	const depth = 6
	p := fanPlan(t, s, e, depth)
	got := countSelectionSets(p.sel)
	// One abstract set and one concrete set per type, per level, with slack
	// for the root and the query type.
	want := fanTypes * (depth + 2) * 2
	if got > want {
		t.Fatalf("plan holds %d selection sets, want at most %d", got, want)
	}
}

// TestFanOutMemoSurvivesDuplicateKeys guards the memo's key. buildField reuses
// first.SelectionSet when a response key has one AST node but appends a fresh
// slice when it has more, so a key derived from slice identity would miss at
// every level here and the expansion would be exponential again.
func TestFanOutMemoSurvivesDuplicateKeys(t *testing.T) {
	s, e := newFanExecutor(t)
	var b strings.Builder
	b.WriteString("{ root { ")
	const depth = 5
	for i := 0; i < depth; i++ {
		// Two selections on the same response key force buildField's merge
		// branch, which allocates a new slice.
		b.WriteString("next { id } next { ")
	}
	b.WriteString("id ")
	b.WriteString(strings.Repeat("} ", depth))
	b.WriteString("} }")

	entry, errs := e.document(b.String())
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	p, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs[0])
	}
	got := countSelectionSets(p.sel)
	want := fanTypes * (depth + 2) * 4
	if got > want {
		t.Fatalf("plan holds %d selection sets, want at most %d", got, want)
	}
}

// TestFanOutQueryCostIsBounded covers the walk that runs per request rather
// than per compile. queryCostOf depends on request variables, so its result
// cannot live on the plan; without its own memo it re-walks every shared
// subgraph of the DAG on every request.
func TestFanOutQueryCostIsBounded(t *testing.T) {
	s, e := newFanExecutor(t, WithQueryCost(QueryCost{Report: true}))
	const depth = 6
	p := fanPlan(t, s, e, depth)

	visits := 0
	var walk func(*selectionSet)
	walk = func(sel *selectionSet) {
		if sel == nil {
			return
		}
		visits++
		for _, sub := range sel.byType {
			walk(sub)
		}
		for _, f := range sel.fields {
			walk(f.sub)
		}
	}
	walk(p.sel)
	// An unmemoized walk of the DAG unfolds it back to the tree. If that is
	// still cheap the fixture is too small to be measuring anything.
	if visits < 10000 {
		t.Fatalf("fixture unfolds to only %d visits; it cannot detect the regression", visits)
	}

	start := time.Now()
	for i := 0; i < 50; i++ {
		if n := queryCostOf(p.sel, nil, QueryCost{}); n <= 0 {
			t.Fatalf("cost = %d, want positive", n)
		}
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("50 cost walks took %v; the walk is not memoized", d)
	}
}

// TestDepthLimitRejectsBeforeCompiling is the point of the guard. Before it,
// WithMaxDepth(3) against this query still built 8^5 selection sets and only
// then reported that the query was three levels too deep.
func TestDepthLimitRejectsBeforeCompiling(t *testing.T) {
	s, e := newFanExecutor(t, WithMaxDepth(3))
	_ = s

	start := time.Now()
	resp := e.Execute(t.Context(), &Request{Query: fanQuery(8)})
	elapsed := time.Since(start)

	if len(resp.Errors) == 0 {
		t.Fatal("want a depth limit error")
	}
	if got := resp.Errors[0].Message; !strings.Contains(got, "maximum depth") {
		t.Fatalf("error = %q, want a maximum depth error", got)
	}
	// Depth 8 is 16 million selection sets unmemoized and tens of thousands
	// memoized. Rejecting without compiling should be neither.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("rejection took %v; the query was compiled before being refused", elapsed)
	}
}
