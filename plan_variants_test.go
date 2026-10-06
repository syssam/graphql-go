package graphql

import (
	"fmt"
	"math/bits"
	"strings"
	"testing"
)

// variantQuery selects n aliases of one field, each behind its own @skip
// variable, so one query text has 2^n plan variants.
func variantQuery(n int) string {
	var b strings.Builder
	b.WriteString("query V(")
	for i := range n {
		fmt.Fprintf(&b, "$v%d: Boolean = false ", i)
	}
	b.WriteString(") { ")
	for i := range n {
		fmt.Fprintf(&b, "s%d: a @skip(if: $v%d) ", i, i)
	}
	b.WriteString("}")
	return b.String()
}

// variantVars sets the variables named by the bits of mask.
func variantVars(n, mask int) string {
	parts := make([]string, 0, n)
	for i := range n {
		parts = append(parts, fmt.Sprintf(`"v%d":%t`, i, mask&(1<<i) != 0))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func variantExecutor(t *testing.T, opts ...ExecutorOption) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(`type Query { a: Int! }`), Query(Field("a", func(Root) int { return 1 })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s, opts...)
}

func storedPlans(d *docEntry) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.plans)
}

// One query text with sixteen @skip variables has 65 536 variants, and every
// one compiled used to be kept for as long as its document stayed cached:
// 2 048 of them held 1.1 GB under an entry the cache counted as 17.8 KB. A
// client needs only to replay one text with different Booleans.
func TestPlanVariantsAreCappedPerDocument(t *testing.T) {
	const n = 8
	e := variantExecutor(t)
	query := variantQuery(n)
	for mask := range 1 << n {
		resp := run(t, e, query, variantVars(n, mask))
		if len(resp.Errors) > 0 {
			t.Fatalf("mask %d: %s", mask, errorsJSON(resp.Errors))
		}
		// A variant past the cap is compiled for the request and not kept; it
		// must still answer what its variables say.
		if got, want := strings.Count(string(resp.Data), `"s`), n-bits.OnesCount(uint(mask)); got != want {
			t.Fatalf("mask %b: %d fields in %s, want %d", mask, got, resp.Data, want)
		}
	}
	entry := e.cache.get(query)
	if entry == nil {
		t.Fatal("the document is not cached")
	}
	if got := storedPlans(entry); got != maxPlanVariants {
		t.Fatalf("%d plans kept for one document, want the cap of %d", got, maxPlanVariants)
	}
	// A kept variant is still a cache hit.
	if _, hit, errs := entry.planFor(e.schema, e, entry.doc.Operations[0], map[string]any{}); errs != nil || !hit {
		t.Fatalf("the first variant compiled is no longer a hit (hit=%t errs=%v)", hit, errs)
	}
}

func noErrors(t *testing.T, resp *Response) {
	t.Helper()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
}

// variantSizes is what the document and each of its first few variants' plans
// are charged, read from an executor with room for all of them.
func variantSizes(t *testing.T, query string, n, variants int) (doc int64, plans []int64) {
	t.Helper()
	e := variantExecutor(t)
	for mask := range variants {
		noErrors(t, run(t, e, query, variantVars(n, mask)))
	}
	entry := e.cache.get(query)
	if entry == nil || storedPlans(entry) != variants {
		t.Fatalf("an executor with the default budget did not keep all %d variants", variants)
	}
	for mask := range variants {
		p := entry.plans[planKey{op: "V", variant: uint16(mask)}]
		if p == nil || p.size <= 0 {
			t.Fatalf("variant %d has no plan with a size", mask)
		}
		plans = append(plans, p.size)
	}
	return documentBytes(entry), plans
}

// The byte budget is what bounds retained memory, and a plan is the larger
// part of what a document retains. Every plan kept is charged what it holds,
// so variants of one text and distinct texts meet the same budget.
func TestPlanVariantsAreChargedToTheByteBudget(t *testing.T) {
	const n = 4
	query := variantQuery(n)
	doc, plans := variantSizes(t, query, n, 4)
	budget := doc + plans[0] + plans[1] + plans[2] + plans[3]
	e := variantExecutor(t, WithPlanCacheBytes(budget))

	for mask := range 1 << n {
		noErrors(t, run(t, e, query, variantVars(n, mask)))
	}
	entry := e.cache.get(query)
	if entry == nil {
		t.Fatal("the document is not cached")
	}
	if got := storedPlans(entry); got != 4 {
		t.Fatalf("%d plans kept under a budget of the document and four plans, want 4", got)
	}
	if got := e.cache.bytes(); got != budget || got != cachedBytes(e.cache) {
		t.Fatalf("cache counts %d bytes and holds %d, want %d: the document and its four plans", got, cachedBytes(e.cache), budget)
	}
	// A variant the budget refused is still answered, and still not kept.
	noErrors(t, run(t, e, query, variantVars(n, 4)))
	if got := storedPlans(entry); got != 4 {
		t.Fatalf("%d plans kept after asking again for one the budget refused", got)
	}

	// Evicting the document gives every charge back.
	other := `{ evict: a }`
	e.cache.mu.Lock()
	for e.cache.lru.Len() > 0 {
		e.cache.removeOldest()
	}
	e.cache.mu.Unlock()
	if got := e.cache.bytes(); got != 0 {
		t.Fatalf("cache counts %d bytes with nothing in it", got)
	}
	noErrors(t, run(t, e, other, ""))
	if got, held := e.cache.bytes(), cachedBytes(e.cache); got != held || got <= documentBytes(e.cache.get(other)) {
		t.Fatalf("cache counts %d bytes for one %d-byte query and its plan, and holds %d", got, len(other), held)
	}
}

// A variant charged to the budget makes room the way a new document does.
func TestPlanVariantChargeEvictsOtherDocuments(t *testing.T) {
	const n = 2
	query := variantQuery(n)
	filler := `{ filler: a }`
	doc, plans := variantSizes(t, query, n, 3)
	probe := variantExecutor(t)
	noErrors(t, run(t, probe, filler, ""))
	fill := probe.cache.bytes()
	// Room for the filler and two plans of the query, and one byte short of
	// a third beside them.
	e := variantExecutor(t, WithPlanCacheBytes(fill+doc+plans[0]+plans[1]+plans[2]-1))

	noErrors(t, run(t, e, filler, ""))
	noErrors(t, run(t, e, query, variantVars(n, 0)))
	noErrors(t, run(t, e, query, variantVars(n, 1)))
	if e.cache.get(filler) == nil {
		t.Fatal("two plans and the filler fit the budget; nothing should be evicted yet")
	}
	noErrors(t, run(t, e, query, variantVars(n, 2)))
	if e.cache.get(filler) != nil {
		t.Fatal("a third plan was kept without evicting the least recently used document")
	}
	if got := storedPlans(e.cache.get(query)); got != 3 {
		t.Fatalf("%d plans kept, want 3: evicting the filler made room for the third", got)
	}
	if got, want := e.cache.bytes(), doc+plans[0]+plans[1]+plans[2]; got != want || got != cachedBytes(e.cache) {
		t.Fatalf("cache counts %d bytes and holds %d, want %d: the document and three plans", got, cachedBytes(e.cache), want)
	}
}

type lockProbe struct{}

// A variant past the cap is compiled for its request and thrown away, on
// every request. Doing that under the document's lock, as the first version
// did, serialised every request for the document behind it, the cached
// variants included; the path for a document with too many conditional
// variables has always compiled outside the lock.
//
// The compile is observed from inside it: a literal custom scalar is decoded
// at plan compile, by the schema's own unmarshal.
func TestAnUncachedVariantIsNotCompiledUnderTheDocumentLock(t *testing.T) {
	var entry *docEntry
	var held, compiles int
	s, err := NewSchema(SDL(`scalar Probe type Query { a: Int! take(v: Probe): Int! }`),
		Scalar("Probe",
			func(w *Writer, _ lockProbe) error { w.Null(); return nil },
			func(any) (lockProbe, error) {
				if entry != nil {
					compiles++
					if entry.mu.TryLock() {
						entry.mu.Unlock()
					} else {
						held++
					}
				}
				return lockProbe{}, nil
			}),
		Args[struct{ V *lockProbe }](),
		Query(
			Field("a", func(Root) int { return 1 }),
			FieldArgs("take", func(Root, struct{ V *lockProbe }) int { return 1 }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)

	const n = 7 // 128 variants against a cap of 64
	query := strings.Replace(variantQuery(n), "}", "take(v: 5) }", 1)
	for mask := range maxPlanVariants {
		noErrors(t, run(t, e, query, variantVars(n, mask)))
	}
	entry = e.cache.get(query)
	if entry == nil || storedPlans(entry) != maxPlanVariants {
		t.Fatalf("the document did not reach its variant cap")
	}

	for mask := maxPlanVariants; mask < 1<<n; mask++ {
		noErrors(t, run(t, e, query, variantVars(n, mask)))
	}
	if compiles == 0 {
		t.Fatal("no variant past the cap was compiled; the probe saw nothing")
	}
	if held != 0 {
		t.Fatalf("%d of %d compiles past the cap ran with the document's lock held", held, compiles)
	}
}

// A plan is kept per value of the variables its @skip and @include read, and
// condVariables is what finds them. One it misses is not part of the key, so
// the plan compiled for the first request answers the second, whatever the
// second sent: every place a condition can be written is asked here with both
// values on one executor, in both orders.
func TestAConditionVariableSelectsItsPlanWhereverItIsWritten(t *testing.T) {
	for name, tc := range map[string]struct{ query, on, off string }{
		"on a field": {
			`query($s: Boolean!) { strings @skip(if: $s) ctxPath }`,
			`{"ctxPath":"ctxPath"}`, `{"strings":["x","y"],"ctxPath":"ctxPath"}`,
		},
		"on a fragment spread": {
			`query($s: Boolean!) { ...F @skip(if: $s) ctxPath } fragment F on Query { strings }`,
			`{"ctxPath":"ctxPath"}`, `{"strings":["x","y"],"ctxPath":"ctxPath"}`,
		},
		"on an inline fragment": {
			`query($s: Boolean!) { ... @skip(if: $s) { strings } ctxPath }`,
			`{"ctxPath":"ctxPath"}`, `{"strings":["x","y"],"ctxPath":"ctxPath"}`,
		},
		"on a field inside a fragment": {
			`query($s: Boolean!) { ...F ctxPath } fragment F on Query { strings @skip(if: $s) }`,
			`{"ctxPath":"ctxPath"}`, `{"strings":["x","y"],"ctxPath":"ctxPath"}`,
		},
		"on an inline fragment inside a fragment": {
			`query($s: Boolean!) { me { ...G } } fragment G on User { ... on User @include(if: $s) { id } name }`,
			`{"me":{"id":"1","name":"Alice"}}`, `{"me":{"name":"Alice"}}`,
		},
		"on a spread inside a fragment": {
			`query($s: Boolean!) { me { ...G } } fragment G on User { ...H @include(if: $s) name } fragment H on User { id }`,
			`{"me":{"id":"1","name":"Alice"}}`, `{"me":{"name":"Alice"}}`,
		},
		"beneath a field of a fragment": {
			`query($s: Boolean!) { ...F } fragment F on Query { me { id @include(if: $s) name } }`,
			`{"me":{"id":"1","name":"Alice"}}`, `{"me":{"name":"Alice"}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, order := range [][]bool{{true, false, true}, {false, true, false}} {
				_, e := newFixtureExecutor(t)
				for _, s := range order {
					want := tc.off
					if s {
						want = tc.on
					}
					expectData(t, run(t, e, tc.query, fmt.Sprintf(`{"s":%t}`, s)), want)
				}
			}
		})
	}
}

// @skip wins when both are present, in either order they are written in.
func TestSkipWinsOverIncludeInEitherOrder(t *testing.T) {
	_, e := newFixtureExecutor(t)
	for _, q := range []string{
		`{ strings @skip(if: true) @include(if: true) ctxPath }`,
		`{ strings @include(if: true) @skip(if: true) ctxPath }`,
		`{ ... @include(if: true) @skip(if: true) { strings } ctxPath }`,
		`{ ...F @include(if: true) @skip(if: true) ctxPath } fragment F on Query { strings }`,
	} {
		expectData(t, run(t, e, q, ""), `{"ctxPath":"ctxPath"}`)
	}
	for _, q := range []string{
		`{ strings @skip(if: false) @include(if: true) ctxPath }`,
		`{ strings @include(if: true) @skip(if: false) ctxPath }`,
	} {
		expectData(t, run(t, e, q, ""), `{"strings":["x","y"],"ctxPath":"ctxPath"}`)
	}
}

// A plan the byte budget has no room for is refused once, under the lock like
// any plan that might be kept, and remembered: what the document is charged
// only grows, so asking again can only be told the same. From then on it is
// compiled outside the lock, as a variant past the cap is.
func TestAPlanTheBudgetRefusedIsCompiledUnderTheLockOnlyOnce(t *testing.T) {
	var entry *docEntry
	var held, compiles int
	s, err := NewSchema(SDL(`scalar Probe type Query { a: Int! take(v: Probe!): Int! }`),
		Scalar("Probe",
			func(w *Writer, _ lockProbe) error { w.Null(); return nil },
			func(any) (lockProbe, error) {
				if entry != nil {
					compiles++
					if entry.mu.TryLock() {
						entry.mu.Unlock()
					} else {
						held++
					}
				}
				return lockProbe{}, nil
			}),
		Args[struct{ V lockProbe }](),
		Query(
			Field("a", func(Root) int { return 1 }),
			FieldArgs("take", func(Root, struct{ V lockProbe }) int { return 1 }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	const query = `{ a take(v: 5) }`
	parsed, errs := NewExecutor(s).document(query)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	// Room for the document and nothing of its plan.
	e := NewExecutor(s, WithPlanCacheBytes(documentBytes(parsed)+1))
	if entry, errs = e.document(query); errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	for range 4 {
		noErrors(t, run(t, e, query, ""))
	}
	if storedPlans(entry) != 0 {
		t.Fatal("a plan larger than the budget was kept")
	}
	if compiles != 4 || held != 1 {
		t.Fatalf("%d compiles, %d of them with the document's lock held; want 4 and only the first", compiles, held)
	}
}
