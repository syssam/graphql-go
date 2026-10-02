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

// The byte budget is what bounds retained memory, and a plan is the larger
// part of what a document retains. Every plan kept beyond a document's first
// is charged the query text again, so variants of one text and distinct texts
// meet the same budget.
func TestPlanVariantsAreChargedToTheByteBudget(t *testing.T) {
	const n = 4
	query := variantQuery(n)
	size := int64(len(query))
	e := variantExecutor(t, WithPlanCacheBytes(4*size))

	for mask := range 1 << n {
		noErrors(t, run(t, e, query, variantVars(n, mask)))
	}
	entry := e.cache.get(query)
	if entry == nil {
		t.Fatal("the document is not cached")
	}
	if got := storedPlans(entry); got != 4 {
		t.Fatalf("%d plans kept under a budget of four times the query text, want 4", got)
	}
	if got := e.cache.bytes(); got != 4*size {
		t.Fatalf("cache counts %d bytes for four plans of a %d-byte query, want %d", got, size, 4*size)
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
	if got := e.cache.bytes(); got != int64(len(other)) {
		t.Fatalf("cache counts %d bytes for one %d-byte query", got, len(other))
	}
}

// A variant charged to the budget makes room the way a new document does.
func TestPlanVariantChargeEvictsOtherDocuments(t *testing.T) {
	const n = 2
	query := variantQuery(n)
	size := int64(len(query))
	filler := `{ filler: a }`
	// Room for three plans of the query, or for two and the filler.
	e := variantExecutor(t, WithPlanCacheBytes(3*size))

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
	if got := e.cache.bytes(); got != 3*size {
		t.Fatalf("cache counts %d bytes for three plans of a %d-byte query", got, size)
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
