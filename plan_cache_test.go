package graphql

import (
	"fmt"
	"strings"
	"testing"
)

// TestPlanCacheEvictsByBytes pins that the byte budget evicts on its own, before
// the entry count is anywhere near its limit.
func TestPlanCacheEvictsByBytes(t *testing.T) {
	c := newPlanCache(100, 10)
	a, b, d := &docEntry{query: "aaaa"}, &docEntry{query: "bbbb"}, &docEntry{query: "cccc"}
	c.put(a)
	c.put(b)
	c.put(d) // 12 bytes against a budget of 10: the least recently used must go.

	if c.get("aaaa") != nil {
		t.Error("aaaa should have been evicted to make room")
	}
	if c.get("bbbb") != b || c.get("cccc") != d {
		t.Error("the two most recent entries should still hit")
	}
	if got := c.bytes(); got > 10 {
		t.Errorf("cache holds %d bytes against a budget of 10", got)
	}
}

// TestPlanCacheSkipsOversizedEntry pins two things at once: a query larger than
// the whole budget is not cached, and trying to make room for it does not evict
// entries that were fine — it can never fit, so clearing space for it would only
// throw away useful documents.
func TestPlanCacheSkipsOversizedEntry(t *testing.T) {
	c := newPlanCache(100, 10)
	small := &docEntry{query: "aaaa"}
	c.put(small)

	c.put(&docEntry{query: strings.Repeat("x", 11)})

	if c.get(strings.Repeat("x", 11)) != nil {
		t.Error("a query larger than the whole budget must not be cached")
	}
	if c.get("aaaa") != small {
		t.Error("an oversized put must not evict entries that fit")
	}
	if got := c.bytes(); got != 4 {
		t.Errorf("bytes = %d, want 4", got)
	}
}

// TestPlanCacheReplacementAdjustsBytes covers the replace branch of put, which
// is where byte accounting leaks if it only ever adds: a colliding query takes
// over the slot, and re-putting the same query must not count it twice.
func TestPlanCacheReplacementAdjustsBytes(t *testing.T) {
	c := newPlanCache(100, 1000)
	c.hash = func(string) uint64 { return 42 }

	c.put(&docEntry{query: "aaaa"})
	c.put(&docEntry{query: "bbbbbbbb"}) // same hash, replaces the slot
	if got := c.bytes(); got != 8 {
		t.Errorf("after a colliding replace, bytes = %d, want 8", got)
	}

	c.put(&docEntry{query: "bbbbbbbb"})
	if got := c.bytes(); got != 8 {
		t.Errorf("after re-putting the same query, bytes = %d, want 8", got)
	}
}

// TestPlanCacheGrowingReplacementEvicts covers a replace that makes its slot
// larger. Adjusting the byte count is not enough on its own: the total can now
// exceed the budget, so other entries have to go.
func TestPlanCacheGrowingReplacementEvicts(t *testing.T) {
	c := newPlanCache(100, 10)
	// Hash on the first byte, so "a..." queries share a slot and "b..." another.
	c.hash = func(s string) uint64 { return uint64(s[0]) }

	c.put(&docEntry{query: "a123"})
	c.put(&docEntry{query: "b123"})
	grown := &docEntry{query: "a12345678"} // replaces a123: 8 - 4 + 9 = 13 bytes
	c.put(grown)

	if got := c.bytes(); got > 10 {
		t.Errorf("cache holds %d bytes against a budget of 10 after a growing replace", got)
	}
	if c.get("b123") != nil {
		t.Error("b123 should have been evicted to bring the cache back within budget")
	}
	if c.get("a12345678") != grown {
		t.Error("the replacing entry itself must survive")
	}
}

// TestPlanCacheZeroBytesIsUnlimited pins the meaning of a zero budget, which
// matches WithMaxDepth's "zero means unlimited" rather than WithPlanCache's
// "zero disables": the entry count still applies, size does not.
func TestPlanCacheZeroBytesIsUnlimited(t *testing.T) {
	c := newPlanCache(2, 0)
	big := &docEntry{query: strings.Repeat("x", 1<<20)}
	c.put(big)
	if c.get(big.query) != big {
		t.Error("with no byte budget a large query must still be cached")
	}
	c.put(&docEntry{query: "a"})
	c.put(&docEntry{query: "b"})
	if c.len() != 2 {
		t.Errorf("len = %d, want the entry count limit of 2 to still apply", c.len())
	}
}

// TestExecutorPlanCacheOptionOrder pins that the two cache options compose in
// either order. WithPlanCache used to build the cache inside the option, so a
// byte budget set before it would have been silently discarded.
func TestExecutorPlanCacheOptionOrder(t *testing.T) {
	_, base := newFixtureExecutor(t)
	schema := base.Schema()
	for name, opts := range map[string][]ExecutorOption{
		"bytes first": {WithPlanCacheBytes(10), WithPlanCache(3)},
		"size first":  {WithPlanCache(3), WithPlanCacheBytes(10)},
	} {
		e := NewExecutor(schema, opts...)
		if e.cache.size != 3 || e.cache.maxBytes != 10 {
			t.Errorf("%s: size=%d maxBytes=%d, want 3 and 10", name, e.cache.size, e.cache.maxBytes)
		}
	}
}

// TestExecutorPlanCacheDefaultBytes pins the default budget. It is set well
// above what the default entry count holds for ordinary queries, so ordinary
// traffic is bounded by the count and only pathologically large queries reach
// the byte budget.
func TestExecutorPlanCacheDefaultBytes(t *testing.T) {
	_, base := newFixtureExecutor(t)
	schema := base.Schema()
	e := NewExecutor(schema)
	if e.cache.size != 1024 {
		t.Errorf("default size = %d, want 1024", e.cache.size)
	}
	if e.cache.maxBytes != 16<<20 {
		t.Errorf("default maxBytes = %d, want 16 MiB", e.cache.maxBytes)
	}
}

// TestPlanCacheBoundsLargeDistinctQueries is the attack the budget exists for:
// many distinct, valid, large queries. Each validates, so each reaches put, and
// a parsed document retains roughly 26 times its query text — so without a
// budget this retains without bound until the entry count saves it, far too
// late. Asserted on the cache's own byte count rather than the heap, which is
// too noisy to bound reliably in a test.
func TestPlanCacheBoundsLargeDistinctQueries(t *testing.T) {
	_, base := newFixtureExecutor(t)
	schema := base.Schema()
	const budget = 64 << 10
	e := NewExecutor(schema, WithPlanCacheBytes(budget))

	sent := 0
	for i := range 40 {
		var q strings.Builder
		fmt.Fprintf(&q, "{ first%d: users { id } ", i)
		for j := 0; q.Len() < 8<<10; j++ {
			fmt.Fprintf(&q, "a%d_%d: users { id name } ", i, j)
		}
		q.WriteString("}")
		sent += q.Len()
		if resp := e.Execute(t.Context(), &Request{Query: q.String()}); len(resp.Errors) != 0 {
			t.Fatalf("query %d: %v", i, resp.Errors[0])
		}
	}

	// Without all three, a cache that stored nothing -- or never updated its
	// byte count -- would satisfy "at most the budget" trivially.
	if sent <= budget {
		t.Fatalf("sent %d bytes, which never exceeds the budget of %d; the test applies no pressure", sent, budget)
	}
	got := e.cache.bytes()
	if got == 0 {
		t.Fatal("cache reports 0 bytes after caching valid queries; its byte count is not being kept")
	}
	if got > budget {
		t.Fatalf("cache holds %d bytes of query text against a budget of %d", got, budget)
	}
}
