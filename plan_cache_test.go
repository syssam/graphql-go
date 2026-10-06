package graphql

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// cachedBytes sums what the cache actually holds -- each document's query text
// and what each plan kept for it retains -- independently of the cache's own
// counter, so a test can tell a counter that drifted from one that is right.
// A bound alone cannot: a counter that undercounts stays within budget.
func cachedBytes(c *planCache) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for el := c.lru.Front(); el != nil; el = el.Next() {
		entry := el.Value.(*cacheItem).entry
		n += int64(len(entry.query))
		entry.mu.Lock()
		for _, p := range entry.plans {
			n += p.size
		}
		entry.mu.Unlock()
	}
	return n
}

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

// TestPlanCacheOversizedCollisionLeavesEntry pins the order of two checks in
// put: the oversized check runs before the hash lookup. Reversed, an oversized
// query sharing a hash with a cached document would replace it and push the
// cache over budget.
func TestPlanCacheOversizedCollisionLeavesEntry(t *testing.T) {
	c := newPlanCache(100, 10)
	c.hash = func(string) uint64 { return 42 }
	kept := &docEntry{query: "aaaa"}
	c.put(kept)

	c.put(&docEntry{query: strings.Repeat("x", 11)}) // same hash, too large

	if c.get("aaaa") != kept {
		t.Error("an oversized query must not replace a cached document it collides with")
	}
	if got, want := c.bytes(), cachedBytes(c); got != 4 || got != want {
		t.Errorf("bytes = %d, held = %d, want both 4", got, want)
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
// a parsed document retained roughly 26 times its query text when measured on
// an alias-heavy query like this one — so without a
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
		t.Fatalf("cache holds %d bytes against a budget of %d", got, budget)
	}
	if held := cachedBytes(e.cache); got != held {
		t.Fatalf("cache counter says %d bytes but holds %d; the counter has drifted", got, held)
	}
}

type wideNode struct{ ID string }

// wideInterfaceExecutor has one interface that every one of n object types
// implements, which is the shape an ORM's Node gives a schema: what a query
// selects on it is planned once for each of them.
func wideInterfaceExecutor(t testing.TB, n int, opts ...ExecutorOption) *Executor {
	t.Helper()
	var sdl strings.Builder
	sdl.WriteString("interface Node { id: ID! }\n")
	options := []SchemaOption{Interface[*wideNode]("Node", TypeResolver(func(*wideNode) string { return "T0" }))}
	for i := range n {
		fmt.Fprintf(&sdl, "type T%d implements Node { id: ID! }\n", i)
		options = append(options, Object[wideNode](fmt.Sprintf("T%d", i), Field("id", func(b *wideNode) ID { return ID(b.ID) })))
	}
	sdl.WriteString("type Query { node: Node }\n")
	options = append(options, Query(Resolve("node", func(context.Context, Root) (*wideNode, error) { return &wideNode{ID: "1"}, nil })))
	s, err := NewSchema(SDL(sdl.String()), options...)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s, opts...)
}

// wideQuery selects aliases fields of the interface, under an operation name
// that makes each text distinct.
func wideQuery(name string, aliases int) string {
	var q strings.Builder
	fmt.Fprintf(&q, "query %s { node {", name)
	for i := range aliases {
		fmt.Fprintf(&q, " a%d: id", i)
	}
	q.WriteString(" } }")
	return q.String()
}

func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// The budget was counted in query text, and a plan is not a function of the
// text: it holds one field for each possible type of what it selects on an
// interface. 990 aliases on an interface with a hundred implementers is
// 8.8 KB of query, under a complexity limit of 1000, and a plan of 19 MB;
// eight of them were 154 MB that the cache reported as 70 KB of a 16 MiB
// budget. A plan is now charged what it retains, so one larger than the whole
// budget is compiled for its request and not kept.
func TestThePlanCacheChargesWhatAPlanRetains(t *testing.T) {
	const types, aliases, queries = 100, 990, 8
	const budget = 16 << 20
	e := wideInterfaceExecutor(t, types, WithMaxComplexity(1000), WithMaxDepth(10), WithPlanCacheBytes(budget))
	before := heapInUse()
	for i := range queries {
		q := wideQuery(fmt.Sprintf("Q%d", i), aliases)
		resp := run(t, e, q, "")
		if len(resp.Errors) > 0 {
			t.Fatalf("query %d: %s", i, errorsJSON(resp.Errors))
		}
		resp.Release()
		entry := e.cache.get(q)
		if entry == nil {
			t.Fatalf("query %d: the document is not cached", i)
		}
		if n := storedPlans(entry); n != 0 {
			t.Fatalf("query %d: %d plans kept, each larger than the whole budget", i, n)
		}
		if i > 0 {
			continue
		}
		// Once is enough to see that it is the budget keeping them out.
		p, hit, errs := entry.planFor(e.schema, e, entry.doc.Operations[0], nil)
		if errs != nil || hit {
			t.Fatalf("hit = %t, errors %v; a plan past the budget is compiled each time it is asked for", hit, errs)
		}
		if p.size <= budget {
			t.Fatalf("the plan is charged %d bytes, which fits the budget; the test applies no pressure", p.size)
		}
	}
	docs, used := e.cache.occupancy()
	if docs != queries || used > budget || used != cachedBytes(e.cache) {
		t.Fatalf("cache holds %d documents charged %d bytes (%d by its contents) against a budget of %d", docs, used, cachedBytes(e.cache), budget)
	}
	// The heap is the point, and it is asked last because it is the noisy
	// one: the bound is a third of what eight kept plans held.
	if !raceEnabled {
		if grew := heapInUse() - before; grew > 48<<20 {
			t.Fatalf("the heap grew by %d MB over %d requests whose plans were not to be kept", grew>>20, queries)
		}
	}
	runtime.KeepAlive(e)
}

// Plans that fit are kept, counted, and evicted by what they hold: a tenth
// of the aliases is a tenth of the plan, and a 16 MiB budget holds a few of
// them where it held the 1024 the entry count allows.
func TestThePlanCacheEvictsByPlanSize(t *testing.T) {
	const types, aliases, queries = 100, 99, 16
	const budget = 16 << 20
	e := wideInterfaceExecutor(t, types, WithPlanCacheBytes(budget))
	var last string
	for i := range queries {
		last = wideQuery(fmt.Sprintf("Q%d", i), aliases)
		resp := run(t, e, last, "")
		if len(resp.Errors) > 0 {
			t.Fatalf("query %d: %s", i, errorsJSON(resp.Errors))
		}
		resp.Release()
	}
	docs, used := e.cache.occupancy()
	if used > budget || used != cachedBytes(e.cache) {
		t.Fatalf("cache is charged %d bytes and holds %d against a budget of %d", used, cachedBytes(e.cache), budget)
	}
	if docs == 0 || docs >= queries {
		t.Fatalf("%d of %d documents are cached: the budget should hold some and not all", docs, queries)
	}
	entry := e.cache.get(last)
	if entry == nil || storedPlans(entry) != 1 {
		t.Fatal("the most recent document's plan was not kept")
	}
	if _, hit, errs := entry.planFor(e.schema, e, entry.doc.Operations[0], nil); errs != nil || !hit {
		t.Fatalf("the most recent plan is not a hit (hit=%t errs=%v)", hit, errs)
	}
}

// The charge is an estimate, and all it has to be is the right size: within
// a factor of the heap a plan is measured to hold, where the query text was
// short of it by four orders of magnitude.
func TestAPlansSizeIsNearWhatItHolds(t *testing.T) {
	if raceEnabled {
		t.Skip("-race changes what an allocation costs")
	}
	e := wideInterfaceExecutor(t, 100)
	entry, errs := e.document(wideQuery("Q", 990))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	before := heapInUse()
	p, errs := compilePlan(e.schema, e, entry.doc, entry.doc.Operations[0], nil)
	if errs != nil {
		t.Fatalf("compile: %v", errs[0])
	}
	held := heapInUse() - before
	t.Logf("the plan is charged %d bytes and holds %d", p.size, held)
	if p.size < held*3/4 || p.size > held*4/3 {
		t.Fatalf("the plan is charged %d bytes and holds %d", p.size, held)
	}
	runtime.KeepAlive(p)
}
