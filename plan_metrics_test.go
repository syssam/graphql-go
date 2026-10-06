package graphql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

// metricsCases covers the constructs where an AST walk and a plan-tree walk
// can disagree: abstract parents taking a max over concrete types, duplicate
// response keys merging, fragments, and skip/include folding. vars is nil
// for cases with no variables; where set, it is decoded into a cond map via
// variantKey (the same path planFor uses), so operationMetrics and the
// compiled plan are compared under the same variant.
//
// fanSDL's own types are Query.root: Node, Node.id: ID! and Node.next: Node —
// no concrete object field, no list, no field with arguments. Every case
// above walks only the abstract-interface shape. "introspection" closes that
// gap: __schema is a concrete object field returning a concrete object,
// fields/args/ofType are lists and nested objects, and args itself is a field
// with arguments in its own right, all reachable because introspection is
// wired into every schema as ordinary bindings.
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
	{
		"introspection",
		`{ __schema { queryType { name fields { name args { name } type { name kind ofType { name } } } } } }`,
		nil,
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
			cond := condValues(entry.condVars, tc.vars)
			want := planMetricsOracle(p)
			got := operationMetrics(s, entry.doc, op, cond)
			if got.complexity != want.complexity {
				t.Errorf("complexity = %d, oracle says %d", got.complexity, want.complexity)
			}
			if got.depth != want.depth {
				t.Errorf("depth = %d, oracle says %d", got.depth, want.depth)
			}
			if got.fields != want.fields {
				t.Errorf("plan fields = %d, the plan holds %d", got.fields, want.fields)
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

// FuzzOperationMetrics is what keeps operationMetrics and the plan-tree oracle
// from drifting. The AST walk reproduces a walk over a structure built by
// different code; nothing but a differential check will notice when one of
// them learns about a construct and the other does not.
func FuzzOperationMetrics(f *testing.F) {
	for _, tc := range metricsCases {
		f.Add(tc.query)
	}
	s, e, err := buildFanExecutor()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, query string) {
		entry, errs := e.document(query)
		if errs != nil {
			return // not a valid document against this schema
		}
		if len(entry.doc.Operations) == 0 {
			return
		}
		op := entry.doc.Operations[0]
		p, perrs := compilePlan(s, e, entry.doc, op, nil)
		if perrs != nil {
			return
		}
		want := planMetricsOracle(p)
		got := operationMetrics(s, entry.doc, op, nil)
		if got != want {
			t.Fatalf("operationMetrics = %+v, oracle = %+v, query = %q", got, want, query)
		}
	})
}

// A refusal is the answer for that operation and variant until the document
// leaves the cache, as a plan would have been. Nothing kept it, so a query
// over the limit was walked again on every request, under the document's
// lock, and the walk is the expensive half of refusing. The stored metrics
// are replaced here so that the second answer shows where it came from.
func TestARefusedOperationIsNotWalkedAgain(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxComplexity(3))
	const q = `query($s: Boolean!) { me { id name friends { id } nick @skip(if: $s) } }`
	refused := func(vars string) any {
		t.Helper()
		resp := run(t, e, q, vars)
		if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeTooComplex {
			t.Fatalf("expected a complexity rejection, got %s", errorsJSON(resp.Errors))
		}
		return resp.Errors[0].Extensions["complexity"]
	}
	if got := refused(`{"s":true}`); got != 5 {
		t.Fatalf("complexity = %v, want 5", got)
	}
	entry := e.cache.get(q)
	if entry == nil {
		t.Fatal("the document is not cached")
	}
	key := planKey{variant: 1}
	entry.mu.Lock()
	_, kept := entry.refused[key]
	if kept {
		entry.refused[key] = planMetrics{complexity: 424242, depth: 3}
	}
	entry.mu.Unlock()
	if !kept {
		t.Fatal("the refusal was not kept with the document")
	}
	if got := refused(`{"s":true}`); got != 424242 {
		t.Fatalf("the second request reported complexity %v: it walked the operation again", got)
	}
	// Per variant, like a plan: the other value of $s is its own answer.
	if got := refused(`{"s":false}`); got != 6 {
		t.Fatalf("complexity with nick included = %v, want 6", got)
	}
}

// One fragment spread under many fields is one selection, however many
// spreads name it. Numbered by spread node, each was its own: the walk
// collected the fragment once per site, sites times fields for a document
// that is their sum, and the plan held as many copies of it.
func TestASpreadIsOneSelectionWhereverItIsWritten(t *testing.T) {
	_, e := newFixtureExecutor(t)
	calls := func(sites int) (int, *docEntry) {
		var q strings.Builder
		q.WriteString("{")
		for i := range sites {
			fmt.Fprintf(&q, " u%d: me { ...F }", i)
		}
		q.WriteString(" } fragment F on User { id name nick }")
		entry, errs := e.document(q.String())
		if errs != nil {
			t.Fatalf("document: %v", errs[0])
		}
		op := entry.doc.Operations[0]
		w := &metricWalker{
			c:    &compiler{s: e.schema, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
			memo: make(map[selKey]planMetrics),
		}
		if m := w.walk(e.schema.rootFor(op.Operation), nil, op.SelectionSet); m.complexity != 4*sites {
			t.Fatalf("complexity = %d, want %d", m.complexity, 4*sites)
		}
		return w.calls, entry
	}
	few, _ := calls(4)
	many, entry := calls(400)
	if many != few {
		t.Fatalf("the walk collected %d selections for 4 sites and %d for 400: one per spread, not one per fragment", few, many)
	}
	p, _, errs := entry.planFor(e.schema, e, entry.doc.Operations[0], nil)
	if errs != nil {
		t.Fatalf("plan: %v", errs[0])
	}
	for _, f := range p.sel.fields[1:] {
		if f.sub != p.sel.fields[0].sub {
			t.Fatalf("%s has its own copy of the fragment's selection", f.alias)
		}
	}
}

// An abstract parent takes the largest of its possible types, and the walk
// visits each of them for it. Once one is over the limit the operation is
// refused whatever the others measure, so a walk that has already done a
// document's worth of collecting stops there: a field with thousands of
// implementers otherwise costs thousands of walks to refuse. The types are
// taken in schema order, so the number reported is the same every time.
func TestOperationMetricsStopsAtTheFirstPossibleTypeOverTheLimit(t *testing.T) {
	s, e := newFanExecutor(t)
	entry, errs := e.document(`{ root { a: id b: id c: id ... on T5 { d: id e: id } } }`)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	op := entry.doc.Operations[0]
	walk := func(maxComplexity, exact int) (planMetrics, int) {
		w := &metricWalker{
			c:             &compiler{s: s, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
			memo:          make(map[selKey]planMetrics),
			maxComplexity: maxComplexity,
			exact:         exact,
		}
		return w.walk(s.rootFor(op.Operation), nil, op.SelectionSet), w.calls
	}
	full, all := walk(0, 0)
	if full.complexity != 6 || all != 1+fanTypes {
		t.Fatalf("unlimited: complexity %d in %d walks, want 6 in %d", full.complexity, all, 1+fanTypes)
	}
	// Over the limit at T0 already: Query and T0 are the two walks.
	for range 20 {
		if m, n := walk(2, 0); m.complexity != 4 || n != 2 {
			t.Fatalf("limit 2: complexity %d in %d walks, want 4 in 2", m.complexity, n)
		}
	}
	// Within it until T5, and nothing after T5 is walked.
	if m, n := walk(4, 0); m.complexity != 6 || n != 1+6 {
		t.Fatalf("limit 4: complexity %d in %d walks, want 6 in 7", m.complexity, n)
	}
	// A limit nothing passes changes nothing.
	if m, n := walk(6, 0); m != full || n != all {
		t.Fatalf("limit 6: %+v in %d walks, want %+v in %d", m, n, full, all)
	}
	// And a walk this small is finished whatever the limit, so the figure
	// an ordinary refusal reports is the operation's own.
	if m, n := walk(2, metricExactWork); m != full || n != all {
		t.Fatalf("limit 2, a small walk: %+v in %d walks, want the exact %+v in %d", m, n, full, all)
	}
}

// The same for a fragment spread under many fields that each add something of
// their own, which keeps them distinct selections: every one collects the
// fragment again, sites times fields. Over the limit after the first, the
// walk stops once it has collected more fields than a document holds.
func TestOperationMetricsStopsCollectingOnceTheOperationIsRefused(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const sites, fields = 400, 400
	var q strings.Builder
	q.WriteString("{")
	for i := range sites {
		fmt.Fprintf(&q, " u%d: me { s%d: id ...F }", i, i)
	}
	q.WriteString(" } fragment F on User {")
	for i := range fields {
		fmt.Fprintf(&q, " a%d: id", i)
	}
	q.WriteString(" }")
	entry, errs := e.document(q.String())
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	op := entry.doc.Operations[0]
	walk := func(maxComplexity int) (planMetrics, *metricWalker) {
		w := &metricWalker{
			c:             &compiler{s: e.schema, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
			memo:          make(map[selKey]planMetrics),
			maxComplexity: maxComplexity,
			exact:         metricExactWork,
		}
		return w.walk(e.schema.rootFor(op.Operation), nil, op.SelectionSet), w
	}
	whole, all := walk(0)
	if want := sites * (fields + 2); whole.complexity != want || all.collected < sites*fields {
		t.Fatalf("unlimited: complexity %d having collected %d fields, want %d and every site", whole.complexity, all.collected, want)
	}
	m, w := walk(100)
	if m.complexity <= 100 {
		t.Fatalf("complexity = %d, which a limit of 100 would let through", m.complexity)
	}
	if most := metricExactWork + fields + sites + 1; w.collected > most {
		t.Fatalf("refused, yet the walk collected %d fields; it should stop within one selection of %d", w.collected, metricExactWork)
	}
}

// A spread's key and a node's key are both written in base 36, where f is a
// digit: tagged with it, fragment 0 was "f0", which is also node 540. A
// document with that many selections, and a single-field set beside a
// single-spread set under one parent, compiled the two as one, and b below
// answered with F's name. Every count near it is tried, since which node is
// number 540 is the walk's business.
func TestASpreadAndANodeNeverShareAFingerprint(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for n := 530; n <= 545; n++ {
		var q strings.Builder
		q.WriteString(`{ a: me { ...F } `)
		for i := range n {
			fmt.Fprintf(&q, "x%d: __typename ", i)
		}
		q.WriteString(`b: me { id } } fragment F on User { name }`)
		resp := run(t, e, q.String(), "")
		if len(resp.Errors) > 0 {
			t.Fatalf("%s", errorsJSON(resp.Errors))
		}
		var data struct {
			A map[string]any `json:"a"`
			B map[string]any `json:"b"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatal(err)
		}
		if _, ok := data.B["id"]; !ok || len(data.B) != 1 {
			t.Fatalf("with %d fields before it, b = %v, want only the id it selected", n, data.B)
		}
		if _, ok := data.A["name"]; !ok || len(data.A) != 1 {
			t.Fatalf("with %d fields before it, a = %v, want only the name F selects", n, data.A)
		}
	}
}
