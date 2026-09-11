package graphql

import (
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

func astSource(q string) *ast.Source { return &ast.Source{Input: q} }

func TestPlanCacheHitAndEviction(t *testing.T) {
	c := newPlanCache(2)
	a, b, d := &docEntry{query: "a"}, &docEntry{query: "b"}, &docEntry{query: "d"}
	c.put(a)
	c.put(b)
	if c.get("a") != a || c.get("b") != b {
		t.Fatal("expected hits")
	}
	c.get("a") // a becomes most recent; b is now the eviction candidate.
	c.put(d)
	if c.get("b") != nil {
		t.Fatal("b should have been evicted")
	}
	if c.get("a") != a || c.get("d") != d || c.len() != 2 {
		t.Fatal("unexpected cache state")
	}
}

func TestPlanCacheCollision(t *testing.T) {
	c := newPlanCache(4)
	c.hash = func(string) uint64 { return 42 }
	a := &docEntry{query: "a"}
	c.put(a)
	if c.get("b") != nil {
		t.Fatal("a colliding query must miss, never return a foreign document")
	}
	if c.get("a") != a {
		t.Fatal("original entry must still hit")
	}
	b := &docEntry{query: "b"}
	c.put(b)
	if c.get("b") != b || c.get("a") != nil || c.len() != 1 {
		t.Fatal("colliding put must replace the slot")
	}
}

func TestPlanCacheDisabled(t *testing.T) {
	c := newPlanCache(0)
	c.put(&docEntry{query: "a"})
	if c.get("a") != nil || c.len() != 0 {
		t.Fatal("zero-size cache must not store")
	}
	var nilCache *planCache
	if nilCache.get("a") != nil {
		t.Fatal("nil cache must miss")
	}
}
