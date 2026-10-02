package apq

import (
	"strconv"
	"testing"
)

// The byte budget is the limit that matters against a client who can register
// anything it can hash, and it counted the query text alone. A cache with no
// entry limit therefore admitted millions of five-byte queries under 16 MiB,
// each holding about 200 bytes of hash, entry and map slot the budget never
// saw: 400 000 of them were 2 MB by its count and 74 MB of heap.
func TestTheByteBudgetCountsWhatAnEntryHoldsBesidesItsText(t *testing.T) {
	const budget = 1 << 20
	c := NewCache(0, WithMaxBytes(budget)).(*lru)
	for i := range 50_000 {
		q := "{a" + strconv.Itoa(i) + "}"
		c.Set(Hash(q), q)
	}
	if held := c.order.Len(); held*entryOverhead > budget {
		t.Fatalf("%d entries held under a %d-byte budget: at %d bytes of overhead each that is %d bytes the budget did not count",
			held, budget, entryOverhead, held*entryOverhead)
	}
	if c.bytes > budget {
		t.Fatalf("cache counts %d bytes against a budget of %d", c.bytes, budget)
	}

	// Evicting everything gives every charge back.
	for c.order.Len() > 0 {
		c.removeOldest()
	}
	if c.bytes != 0 {
		t.Fatalf("cache counts %d bytes with nothing in it", c.bytes)
	}
}
