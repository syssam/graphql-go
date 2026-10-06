package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

// planFieldsOf counts the planFields a compiled plan holds, each selection set
// once however many parents share it: the number WithMaxPlanFields bounds,
// read off the thing itself.
func planFieldsOf(sel *selectionSet) int {
	seen := make(map[*selectionSet]bool)
	var count func(*selectionSet) int
	count = func(s *selectionSet) int {
		if s == nil || seen[s] {
			return 0
		}
		seen[s] = true
		n := len(s.fields)
		for _, f := range s.fields {
			n += count(f.sub)
		}
		for _, concrete := range s.byType {
			n += count(concrete)
		}
		return n
	}
	return count(sel)
}

func refusalOf(t *testing.T, resp *Response) *Error {
	t.Helper()
	if !resp.HasRequestErrors() || len(resp.Errors) != 1 {
		t.Fatalf("expected one request error, got %d bytes of data and errors %s", len(resp.Data), errorsJSON(resp.Errors))
	}
	return resp.Errors[0]
}

// The complexity limit counts what the query selects, and a plan holds that
// once for each concrete type an interface could be: 990 aliases are under a
// limit of 1000 whatever the interface has for implementers. This is the
// limit on the product.
func TestMaxPlanFieldsRefusesWhatTheComplexityLimitLetsThrough(t *testing.T) {
	const types = 100
	e := wideInterfaceExecutor(t, types, WithMaxComplexity(1000), WithMaxDepth(10), WithMaxPlanFields(10_000))

	// Query.node and 99 aliases on each of a hundred types.
	within := wideQuery("Within", 99)
	resp := run(t, e, within, "")
	noErrors(t, resp)
	resp.Release()

	wide := wideQuery("Wide", 990)
	err := refusalOf(t, run(t, e, wide, ""))
	if err.Extensions["code"] != CodeTooComplex {
		t.Fatalf("code = %v, want %s", err.Extensions["code"], CodeTooComplex)
	}
	if err.Extensions["maxPlanFields"] != 10_000 {
		t.Fatalf("maxPlanFields = %v, want 10000", err.Extensions["maxPlanFields"])
	}
	// The count stopped where a walk stops owing an exact figure, so what it
	// reports is how far it got and not the plan's 99 001.
	counted, _ := err.Extensions["planFields"].(int)
	if counted <= 10_000 || counted > metricExactWork+990+1 {
		t.Fatalf("planFields = %v, want past the limit and within a selection of %d", err.Extensions["planFields"], metricExactWork)
	}
	if !strings.Contains(err.Message, "10000") || !strings.Contains(err.Message, fmt.Sprint(counted)) {
		t.Fatalf("the message names neither the limit nor the count: %q", err.Message)
	}

	entry := e.cache.get(wide)
	if entry == nil {
		t.Fatal("the document is not cached")
	}
	entry.mu.Lock()
	plans := len(entry.plans)
	m, kept := entry.refused[planKey{op: "Wide"}]
	if kept {
		m.fields = 424242
		entry.refused[planKey{op: "Wide"}] = m
	}
	entry.mu.Unlock()
	if plans != 0 {
		t.Fatalf("%d plans kept for a refused operation", plans)
	}
	if !kept {
		t.Fatal("the refusal was not kept with the document")
	}
	if got := refusalOf(t, run(t, e, wide, "")).Extensions["planFields"]; got != 424242 {
		t.Fatalf("the second request reported %v plan fields: it walked the operation again", got)
	}
	// Refused before it was planned, so it is neither a hit nor a miss.
	if st := e.Stats(); st.PlanCacheHits != 0 || st.PlanCacheMisses != 1 {
		t.Fatalf("hits = %d, misses = %d, want 0 and the one operation that was planned", st.PlanCacheHits, st.PlanCacheMisses)
	}
}

// A refusal that stopped a small walk would report how far it got. An
// ordinary operation over the limit is told its own number, and one field
// fewer is let through.
func TestMaxPlanFieldsIsExactAtTheBoundary(t *testing.T) {
	const types = 100
	q := wideQuery("Q", 99)
	const fields = 1 + 99*types

	at := wideInterfaceExecutor(t, types, WithMaxPlanFields(fields))
	resp := run(t, at, q, "")
	noErrors(t, resp)
	resp.Release()

	under := wideInterfaceExecutor(t, types, WithMaxPlanFields(fields-1))
	err := refusalOf(t, run(t, under, q, ""))
	if err.Extensions["planFields"] != fields || err.Extensions["maxPlanFields"] != fields-1 {
		t.Fatalf("planFields = %v against %v, want %d against %d", err.Extensions["planFields"], err.Extensions["maxPlanFields"], fields, fields-1)
	}

	// Half way through a walk this small the count is already past the
	// limit, and the walk is finished for the number all the same.
	half := wideInterfaceExecutor(t, types, WithMaxPlanFields(fields/2))
	if err := refusalOf(t, run(t, half, q, "")); err.Extensions["planFields"] != fields {
		t.Fatalf("planFields = %v under a limit of %d, want the plan's own %d", err.Extensions["planFields"], fields/2, fields)
	}

	// Zero is no limit, as it is for depth and complexity.
	off := wideInterfaceExecutor(t, types, WithMaxPlanFields(0))
	resp = run(t, off, wideQuery("Wide", 990), "")
	noErrors(t, resp)
	resp.Release()
}

// planFieldCases are the shapes where a count taken from the document could
// part from what the compiler builds: possible types an inline fragment does
// and does not apply to, a selection two parents share, response keys that
// merge, and what a directive leaves out of one variant and not another.
var planFieldCases = []struct {
	name  string
	query string
	vars  []string
}{
	{"plain objects", `{ me { id name friends { id name bestFriend { id } } } }`, nil},
	{"one response key written three times", `{ me { id id id name: id } }`, nil},
	{"merged response keys", `{ me { friends { id } friends { name } } }`, nil},
	{"typename", `{ __typename me { __typename id } }`, nil},
	{"arguments", `{ user(id: "1") { id } users(filter: {name: "a"}) { name } echo(v: 1) }`, nil},
	{"interface", `{ node(id: "1") { id } }`, nil},
	{"interface narrowed to some types", `{ node(id: "1") { id ... on User { name nick } ... on Post { title } } }`, nil},
	{"interface narrowed to one type only", `{ node(id: "1") { ... on Post { title author { name } } } }`, nil},
	{"union", `{ search(term: "a") { __typename ... on User { name } ... on Post { title author { name } } } }`, nil},
	{"union through an interface condition", `{ search(term: "a") { ... on Node { id } } }`, nil},
	{"nested abstract fields", `{ me { pet { name ... on Dog { barks } ... on Cat { lives } } friends { pet { name } } } }`, nil},
	{"abstract under abstract", `{ search(term: "a") { ... on User { pet { name ... on Cat { lives } } } ... on Post { author { pet { name } } } } }`, nil},
	{"a fragment under two parents", `{ me { ...U } user(id: "2") { ...U } } fragment U on User { id name bestFriend { id } }`, nil},
	{"a fragment under two parent types", `{ me { ...N } node(id: "1") { ...N } search(term: "a") { ...N } } fragment N on Node { id }`, nil},
	{"a fragment beside a field of its own", `{ me { a: id ...U } user(id: "2") { b: id ...U } } fragment U on User { name friends { id } }`, nil},
	{"fragments spreading fragments", `{ me { ...A } } fragment A on User { id ...B friends { ...B } } fragment B on User { name pet { name } }`, nil},
	{"literal conditions", `{ me { id @skip(if: true) name @include(if: false) nick friends @skip(if: false) { id } } }`, nil},
	{
		"conditions on variables",
		`query($s: Boolean!, $i: Boolean!) { me { id name @skip(if: $s) ...U @include(if: $i) pet @skip(if: $s) { name } } node(id: "1") { ... on User @include(if: $i) { nick } } } fragment U on User { nick tags friends { id @skip(if: $s) name } }`,
		[]string{`{"s":false,"i":false}`, `{"s":true,"i":false}`, `{"s":false,"i":true}`, `{"s":true,"i":true}`},
	},
	{"introspection", `{ __schema { queryType { name fields { name args { name } type { name kind ofType { name } } } } } __type(name: "User") { name } me { id } }`, nil},
	{"the introspection query a tool sends", graphiqlIntrospection, nil},
}

// The limit is on what the plan holds, so the count has to be the plan's:
// taken from the document before the plan exists, and then held to the plan
// that is compiled. One field more than the limit is refused with the plan's
// own number, and the limit set to it lets the operation through.
func TestThePlanFieldCountIsThePlans(t *testing.T) {
	for _, tc := range planFieldCases {
		t.Run(tc.name, func(t *testing.T) {
			_, e := newFixtureExecutor(t)
			entry, errs := e.document(tc.query)
			if errs != nil {
				t.Fatalf("document: %v", errs[0])
			}
			op := entry.doc.Operations[0]
			variants := tc.vars
			if variants == nil {
				variants = []string{""}
			}
			seen := make(map[int]bool)
			for _, vars := range variants {
				var decoded map[string]any
				if vars != "" {
					if err := json.Unmarshal([]byte(vars), &decoded); err != nil {
						t.Fatal(err)
					}
				}
				p, _, perrs := entry.planFor(e.schema, e, op, decoded)
				if perrs != nil {
					t.Fatalf("plan: %v", perrs[0])
				}
				want := planFieldsOf(p.sel)
				seen[want] = true
				got := operationMetrics(e.schema, entry.doc, op, condValues(entry.condVars, decoded))
				if got.fields != want {
					t.Fatalf("vars %s: counted %d plan fields before compiling, and the plan holds %d", vars, got.fields, want)
				}

				_, at := newFixtureExecutor(t, WithMaxPlanFields(want))
				resp := run(t, at, tc.query, vars)
				noErrors(t, resp)
				resp.Release()
				_, under := newFixtureExecutor(t, WithMaxPlanFields(want-1))
				if err := refusalOf(t, run(t, under, tc.query, vars)); err.Extensions["planFields"] != want {
					t.Fatalf("vars %s: refused at %v plan fields, and the plan holds %d", vars, err.Extensions["planFields"], want)
				}
			}
			if len(variants) > 1 && len(seen) < 3 {
				t.Fatalf("the variants hold %v fields: they do not differ enough to tell one count from another", seen)
			}
		})
	}
}

// The same against the schema the limit is for: every possible type is a
// selection set of its own, and nested they share one.
func TestThePlanFieldCountIsThePlansOnAWideInterface(t *testing.T) {
	s, e := newFanExecutor(t)
	for depth := range 5 {
		entry, errs := e.document(fanQuery(depth))
		if errs != nil {
			t.Fatalf("document: %v", errs[0])
		}
		op := entry.doc.Operations[0]
		p, _, perrs := entry.planFor(s, e, op, nil)
		if perrs != nil {
			t.Fatalf("plan: %v", perrs[0])
		}
		want := planFieldsOf(p.sel)
		// root, then id and next on each type at each level but the last.
		if formula := 1 + fanTypes*(2*depth+1); want != formula {
			t.Fatalf("depth %d: the plan holds %d fields, want %d", depth, want, formula)
		}
		if got := operationMetrics(s, entry.doc, op, nil).fields; got != want {
			t.Fatalf("depth %d: counted %d plan fields, and the plan holds %d", depth, got, want)
		}
	}
}

// refusalWalk measures a 990-alias query on an interface with types
// implementers against a plan field limit, as compile does, and returns the
// walker for what it did.
func refusalWalk(t *testing.T, types, maxFields int) *metricWalker {
	t.Helper()
	e := wideInterfaceExecutor(t, types, WithMaxComplexity(1000), WithMaxDepth(10))
	entry, errs := e.document(wideQuery("Q", 990))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	op := entry.doc.Operations[0]
	w := &metricWalker{
		c:             &compiler{s: e.schema, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
		memo:          make(map[selKey]planMetrics),
		maxComplexity: 1000,
		maxDepth:      10,
		maxFields:     maxFields,
		exact:         metricExactWork,
	}
	if m := w.walk(e.schema.rootFor(op.Operation), nil, op.SelectionSet); m.complexity > 1000 || m.depth > 10 {
		t.Fatalf("%+v: the complexity and depth limits were to let this through", m)
	}
	return w
}

// What the limit is for costs 4.6 s and 952 MB to plan on an interface with
// five thousand implementers, and a refusal that measured all of it first
// would have refused nothing. The count stops once it is past the limit, so
// refusing costs what the limit allows whatever the plan would have been:
// the same walk for five thousand types as for one thousand, and for a
// limit ten times smaller a tenth of it.
func TestRefusingAPlanCostsTheLimitNotThePlan(t *testing.T) {
	const aliases, limit = 990, 100_000
	steps := func(types, maxFields int) (calls, collected int) {
		w := refusalWalk(t, types, maxFields)
		if w.fields <= maxFields {
			t.Fatalf("%d types: counted %d plan fields, which a limit of %d lets through", types, w.fields, maxFields)
		}
		return w.calls, w.collected
	}
	// Query, then one type at a time until the count passes the limit.
	wantCalls := 1 + (limit+aliases-1)/aliases
	for _, types := range []int{1000, 5000} {
		calls, collected := steps(types, limit)
		t.Logf("%d types: refused after %d selections and %d fields collected, of the %d a plan holds", types, calls, collected, 1+aliases*types)
		if calls != wantCalls {
			t.Fatalf("%d types: the walk visited %d selections to refuse, want %d: it should stop at the type that passes the limit", types, calls, wantCalls)
		}
		if most := limit + 2*aliases; collected > most {
			t.Fatalf("%d types: the walk collected %d fields to refuse at %d", types, collected, limit)
		}
	}
	if calls, _ := steps(5000, limit/10); calls > wantCalls/5 {
		t.Fatalf("a limit of %d took %d selections to refuse where %d took %d", limit/10, calls, limit, wantCalls)
	}

	// And nothing of the plan is built on the way. Collecting a field is two
	// allocations and building it into a plan several more, so what a refusal
	// allocates is the walk's -- the same for either schema.
	refusal := func(types int) float64 {
		e := wideInterfaceExecutor(t, types, WithMaxComplexity(1000), WithMaxDepth(10), WithMaxPlanFields(limit))
		entry, errs := e.document(wideQuery("Q", aliases))
		if errs != nil {
			t.Fatalf("document: %v", errs[0])
		}
		op := entry.doc.Operations[0]
		return testing.AllocsPerRun(1, func() {
			if _, _, errs := entry.compile(e.schema, e, op, nil, planKey{}, false); errs == nil {
				t.Fatal("the operation was compiled")
			}
		})
	}
	small, large := refusal(1000), refusal(5000)
	t.Logf("refusing allocates %.0f objects with 1000 types and %.0f with 5000", small, large)
	if large > 3*limit {
		t.Fatalf("refusing at %d fields allocated %.0f objects: the plan is being built before it is refused", limit, large)
	}
	if large > small*1.01 {
		t.Fatalf("refusing allocated %.0f objects with 1000 types and %.0f with 5000: it grows with the plan", small, large)
	}
}

// With no limit the walk is the one it always was: every possible type.
func TestWithNoPlanFieldLimitTheWalkIsWhole(t *testing.T) {
	const types = 200
	w := refusalWalk(t, types, 0)
	if w.calls != 1+types || w.fields != 1+990*types {
		t.Fatalf("visited %d selections and counted %d fields, want %d and %d", w.calls, w.fields, 1+types, 1+990*types)
	}
}

// Introspection is free to the depth and complexity limits and the walk does
// not stop inside it for them. Its fields are in the plan all the same, so a
// walk that has collected a document's worth and is over a complexity limit
// only within __schema still counts every one.
func TestThePlanFieldCountDoesNotStopInsideIntrospectionForAnotherLimit(t *testing.T) {
	_, e := newFixtureExecutor(t)
	entry, errs := e.document(`{ __schema { types { fields { name type { name } } name interfaces { name } possibleTypes { name } } } me { id } }`)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	op := entry.doc.Operations[0]
	p, errs := compilePlan(e.schema, e, entry.doc, op, nil)
	if errs != nil {
		t.Fatalf("compile: %v", errs[0])
	}
	w := &metricWalker{
		c:             &compiler{s: e.schema, doc: entry.doc, nodeID: make(map[ast.Selection]int32)},
		memo:          make(map[selKey]planMetrics),
		maxComplexity: 3,
		maxFields:     math.MaxInt,
	}
	m := w.walk(e.schema.rootFor(op.Operation), nil, op.SelectionSet)
	if m.complexity != 3 {
		t.Fatalf("complexity = %d, want 3: __schema, me and id", m.complexity)
	}
	if want := planFieldsOf(p.sel); w.fields != want {
		t.Fatalf("counted %d plan fields with a complexity limit set, and the plan holds %d", w.fields, want)
	}
}

func TestMaxPlanFieldsAppliesToSubscriptions(t *testing.T) {
	const q = `subscription { messages { id body } }`

	src, e := newSubExecutor(t, WithMaxPlanFields(2))
	ch, err := e.Subscribe(context.Background(), &Request{Query: q})
	var se *SubscribeError
	if !errors.As(err, &se) || ch != nil {
		t.Fatalf("a subscription of three plan fields was opened under a limit of two (err = %v)", err)
	}
	refused := refusalOf(t, se.Response)
	if refused.Extensions["code"] != CodeTooComplex || refused.Extensions["planFields"] != 3 {
		t.Fatalf("refused with %s, want %s for 3 plan fields", errorsJSON(se.Response.Errors), CodeTooComplex)
	}
	if n := src.opens.Load(); n != 0 {
		t.Fatalf("the source was opened %d times for a refused subscription", n)
	}

	_, e = newSubExecutor(t, WithMaxPlanFields(3))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.Subscribe(ctx, &Request{Query: q}); err != nil {
		t.Fatalf("a subscription of three plan fields was refused under a limit of three: %v", err)
	}
}

// The limit bounds what a plan holds, not what a request may ask a resolver
// to do, so it is not one of the limits whose absence the executor reports.
func TestAPlanFieldLimitAloneLeavesRequestsUnbounded(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxPlanFields(1000))
	if !e.Stats().RequestsUnbounded {
		t.Fatal("RequestsUnbounded is false with no depth, complexity or cost limit set")
	}
	_, e = newFixtureExecutor(t, WithMaxPlanFields(1000), WithMaxDepth(5))
	if e.Stats().RequestsUnbounded {
		t.Fatal("RequestsUnbounded is true with a depth limit set")
	}
}
