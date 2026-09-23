package graphql

import (
	"strings"
	"testing"
)

// The plan cache's two bounds are documented options and neither had a test.
// Nothing about them is visible in a response -- a cache that caches when told
// not to still answers correctly -- so the only thing that would ever notice is
// Stats, which is also the only thing an operator has. Someone who disables the
// cache is doing it for a reason, usually memory, and would have no way to tell
// it had not happened.

func planCacheExec(t *testing.T, opts ...ExecutorOption) *Executor {
	t.Helper()
	_, e := newFixtureExecutor(t, opts...)
	return e
}

// WithPlanCache(0): "Zero disables caching." Without the size guard in put,
// the eviction loop leaves exactly one entry rather than none -- still wrong,
// still invisible, and it holds a document the operator asked not to keep.
func TestWithPlanCacheZeroKeepsNothing(t *testing.T) {
	e := planCacheExec(t, WithPlanCache(0))
	for _, q := range []string{`{ me { id } }`, `{ me { name } }`, `{ me { id name } }`} {
		if resp := run(t, e, q, ""); len(resp.Errors) > 0 {
			t.Fatalf("%s: %s", q, errorsJSON(resp.Errors))
		}
	}
	st := e.Stats()
	if st.PlanCacheEntries != 0 || st.PlanCacheBytes != 0 {
		t.Errorf("cache holds %d entries / %d bytes after three queries with caching "+
			"disabled", st.PlanCacheEntries, st.PlanCacheBytes)
	}
	// And the same query twice is still two misses, which is what "disabled"
	// means to anyone reading the hit rate.
	run(t, e, `{ me { id } }`, "")
	if st = e.Stats(); st.PlanCacheHits != 0 {
		t.Errorf("PlanCacheHits = %d with caching disabled", st.PlanCacheHits)
	}
}

// With caching on, the same document is a hit and a different one is a miss.
// This is the control: without it the test above passes on an executor that
// never caches anything for some unrelated reason.
func TestPlanCacheCountsHitsAndMisses(t *testing.T) {
	e := planCacheExec(t, WithPlanCache(16))
	run(t, e, `{ me { id } }`, "")
	run(t, e, `{ me { id } }`, "")
	run(t, e, `{ me { name } }`, "")
	st := e.Stats()
	if st.PlanCacheEntries != 2 {
		t.Errorf("PlanCacheEntries = %d, want 2", st.PlanCacheEntries)
	}
	if st.PlanCacheHits != 1 || st.PlanCacheMisses != 2 {
		t.Errorf("hits/misses = %d/%d, want 1/2", st.PlanCacheHits, st.PlanCacheMisses)
	}
	if st.PlanCacheBytes == 0 {
		t.Error("PlanCacheBytes = 0 with two documents cached")
	}
}

// WithPlanCacheBytes bounds the query text held, evicting to stay within it.
// A document larger than the whole budget is never stored, because evicting to
// make room for it would discard documents that do fit.
func TestPlanCacheBytesEvictsAndRefusesAnOversizedDocument(t *testing.T) {
	// Room for a couple of small documents, not for many.
	e := planCacheExec(t, WithPlanCache(64), WithPlanCacheBytes(120))
	for _, q := range []string{
		`{ me { id } }`,
		`{ me { name } }`,
		`{ me { id name } }`,
		`{ me { id } }`,
		`{ me { name } }`,
	} {
		if resp := run(t, e, q, ""); len(resp.Errors) > 0 {
			t.Fatalf("%s: %s", q, errorsJSON(resp.Errors))
		}
	}
	st := e.Stats()
	if st.PlanCacheBytes > 120 {
		t.Errorf("PlanCacheBytes = %d, over the 120-byte budget", st.PlanCacheBytes)
	}
	if st.PlanCacheEntries == 0 {
		t.Error("the byte budget evicted everything; it should hold what fits")
	}

	// A document past the whole budget on its own is refused rather than
	// emptying the cache to fail anyway.
	before := e.Stats()
	big := "{ me { " + strings.Repeat("id ", 200) + "} }"
	if resp := run(t, e, big, ""); len(resp.Errors) > 0 {
		t.Fatalf("oversized query: %s", errorsJSON(resp.Errors))
	}
	after := e.Stats()
	if after.PlanCacheEntries < before.PlanCacheEntries {
		t.Errorf("a document too large to cache evicted %d that fit",
			before.PlanCacheEntries-after.PlanCacheEntries)
	}
	if after.PlanCacheBytes > 120 {
		t.Errorf("PlanCacheBytes = %d after the oversized document", after.PlanCacheBytes)
	}
}
