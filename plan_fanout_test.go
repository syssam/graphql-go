package graphql

import (
	"encoding/json"
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

// buildFanExecutor builds the fan-out fixture schema without a *testing.T, so
// a fuzz target can report a schema failure as a clean skip rather than a
// panic from calling t.Fatal on a bare &testing.T{}.
func buildFanExecutor(opts ...ExecutorOption) (*Schema, *Executor, error) {
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
		return nil, nil, err
	}
	return s, NewExecutor(s, opts...), nil
}

func newFanExecutor(t *testing.T, opts ...ExecutorOption) (*Schema, *Executor) {
	t.Helper()
	s, e, err := buildFanExecutor(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s, e
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

// TestDepthLimitRejectsWithoutCompiling is the point of the guard, asserted
// directly rather than through a latency budget. An earlier version of this
// test timed the rejection instead, on the theory that a compiled-then-
// rejected query would be slow: true before Task 2 (1.65s for a depth-5
// fan-out, measured pre-memoization), but Task 2's DAG memoization already
// bounds compilePlan for this fixture to about fanTypes*depth selection
// sets, so both the guarded and unguarded paths finish in well under a
// millisecond here and a wall-clock assertion cannot tell them apart — it
// passed whether or not the guard ran. What "rejects before compiling"
// actually means is structural: no plan exists afterward. That is what this
// test checks.
func TestDepthLimitRejectsWithoutCompiling(t *testing.T) {
	s, e := newFanExecutor(t, WithMaxDepth(3))
	_ = s

	query := fanQuery(8)
	entry, errs := e.document(query)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}

	resp := e.Execute(t.Context(), &Request{Query: query})
	if len(resp.Errors) == 0 {
		t.Fatal("want a depth limit error")
	}
	if got := resp.Errors[0].Message; !strings.Contains(got, "maximum depth") {
		t.Fatalf("error = %q, want a maximum depth error", got)
	}

	// d.plans is written under d.mu; reading it unlocked here would be the
	// data race its own comment in plan.go warns about, even though this
	// goroutine is the only writer in this test.
	entry.mu.Lock()
	n := len(entry.plans)
	entry.mu.Unlock()
	if n != 0 {
		t.Fatalf("plans cached for rejected operation = %d, want 0: a depth-rejected query must never be compiled or cached", n)
	}
}

// TestQueryCostAbsentOnDepthRejection pins the observable change recorded in
// docs/superpowers/specs/2026-09-16-plan-expansion-blowup-design.md, section
// 7 ("Observable Changes"): a query rejected by the pre-compile depth or
// complexity guard never carries extensions.cost. attachCost (limits.go)
// requires oc.plan, and execute returns via requestError before an
// OperationContext is ever built when the guard fires, so attachCost never
// runs. This is deliberate, not a regression to fix — QueryCost.Max
// rejections are unaffected, since that check stays in rejectIfOverLimit,
// after compilation, because it depends on request variables.
func TestQueryCostAbsentOnDepthRejection(t *testing.T) {
	s, e := newFanExecutor(t, WithMaxDepth(3), WithQueryCost(QueryCost{Report: true}))
	_ = s

	resp := e.Execute(t.Context(), &Request{Query: fanQuery(8)})
	if len(resp.Errors) == 0 {
		t.Fatal("want a depth limit error")
	}
	if got := resp.Errors[0].Message; !strings.Contains(got, "maximum depth") {
		t.Fatalf("error = %q, want a maximum depth error", got)
	}
	if _, ok := resp.Extensions["cost"]; ok {
		t.Fatalf("extensions.cost = %v, want absent: a guard rejection never compiles a plan for attachCost to read", resp.Extensions["cost"])
	}
}

// TestDepthLimitRejectsOnUncacheablePath exercises the guard's other branch:
// a document with more than maxCondVars boolean variables bound to
// @skip/@include never enters the plan cache (docEntry.planUncacheable), so
// every planFor call takes the store=false path through d.compile. There is
// no d.plans to inspect on this branch — store is false regardless of
// whether the guard fires — so this test cannot pin "no plan was cached" the
// way TestDepthLimitRejectsWithoutCompiling does for the cached path. What is
// observable, and what this test checks instead, is the same absence
// TestQueryCostAbsentOnDepthRejection checks: a guard rejection never builds
// an OperationContext, so extensions.cost never appears, whereas a rejection
// caught only by the post-compile rejectIfOverLimit check still reports it.
func TestDepthLimitRejectsOnUncacheablePath(t *testing.T) {
	s, e := newFanExecutor(t, WithMaxDepth(3), WithQueryCost(QueryCost{Report: true}))
	_ = s

	var b strings.Builder
	b.WriteString("query TooManyCond(")
	for i := 0; i <= maxCondVars; i++ {
		fmt.Fprintf(&b, "$v%d: Boolean!, ", i)
	}
	b.WriteString(") { root { id ")
	for i := 0; i <= maxCondVars; i++ {
		fmt.Fprintf(&b, "a%d: id @skip(if: $v%d) ", i, i)
	}
	const depth = 3
	for i := 0; i < depth; i++ {
		b.WriteString("next { id ")
	}
	b.WriteString(strings.Repeat("} ", depth))
	b.WriteString("} }")
	query := b.String()

	entry, errs := e.document(query)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	if !entry.planUncacheable() {
		t.Fatalf("condVars = %d, want more than %d to exercise the uncacheable path", len(entry.condVars), maxCondVars)
	}

	vars := make(map[string]any, maxCondVars+1)
	for i := 0; i <= maxCondVars; i++ {
		vars[fmt.Sprintf("v%d", i)] = false
	}
	varsJSON, err := json.Marshal(vars)
	if err != nil {
		t.Fatal(err)
	}

	resp := e.Execute(t.Context(), &Request{Query: query, Variables: varsJSON})
	if len(resp.Errors) == 0 {
		t.Fatal("want a depth limit error")
	}
	if got := resp.Errors[0].Message; !strings.Contains(got, "maximum depth") {
		t.Fatalf("error = %q, want a maximum depth error", got)
	}
	if _, ok := resp.Extensions["cost"]; ok {
		t.Fatalf("extensions.cost = %v, want absent on the uncacheable path too", resp.Extensions["cost"])
	}
}
