package graphql

import (
	"container/list"
	"hash/maphash"
	"sync"
)

// planCache is an LRU of parsed documents keyed by a 64-bit hash of the query
// text. Because two queries may hash alike, a hit is confirmed by comparing
// the stored query string, so a collision degrades to a miss.
type planCache struct {
	mu    sync.Mutex
	size  int
	items map[uint64]*list.Element
	lru   *list.List
	hash  func(string) uint64
}

type cacheItem struct {
	hash  uint64
	entry *docEntry
}

func newPlanCache(size int) *planCache {
	seed := maphash.MakeSeed()
	return &planCache{
		size:  size,
		items: make(map[uint64]*list.Element, size),
		lru:   list.New(),
		hash:  func(s string) uint64 { return maphash.String(seed, s) },
	}
}

// get returns the cached document for query, or nil.
func (c *planCache) get(query string) *docEntry {
	if c == nil || c.size <= 0 {
		return nil
	}
	h := c.hash(query)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[h]
	if !ok {
		return nil
	}
	item := el.Value.(*cacheItem)
	if item.entry.query != query {
		return nil
	}
	c.lru.MoveToFront(el)
	return item.entry
}

// put stores entry, evicting the least recently used document when full.
func (c *planCache) put(entry *docEntry) {
	if c == nil || c.size <= 0 {
		return
	}
	h := c.hash(entry.query)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[h]; ok {
		el.Value.(*cacheItem).entry = entry
		c.lru.MoveToFront(el)
		return
	}
	for c.lru.Len() >= c.size {
		oldest := c.lru.Back()
		c.lru.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheItem).hash)
	}
	c.items[h] = c.lru.PushFront(&cacheItem{hash: h, entry: entry})
}

// len reports the number of cached documents.
func (c *planCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}
