// Package apq implements automatic persisted queries.
//
// A client sends a hash instead of the query text. If the server knows the
// hash it executes the stored query; if not it answers PersistedQueryNotFound
// and the client retries with the text, which the server verifies and stores.
// Steady state is a small request that a GET can carry and a CDN can cache.
//
// Resolution has to happen before a transport decides what an operation is:
// a request carrying only a hash has no query text, so a check such as
// "mutations are not allowed over GET" would see nothing to inspect and wave
// it through. The HTTP transports therefore resolve as part of parsing rather
// than at execution time.
package apq

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/syssam/graphql-go"
)

// Error codes carried in extensions.code.
const (
	CodeNotFound     = "PERSISTED_QUERY_NOT_FOUND"
	CodeNotSupported = "PERSISTED_QUERY_NOT_SUPPORTED"
)

// Cache stores query text by hash. Implementations must be safe for
// concurrent use; every request touches the cache.
//
// A cache shared between processes (Redis, memcached) is a valid
// implementation and is what makes a fleet warm rather than each instance
// learning hashes separately.
type Cache interface {
	Get(hash string) (query string, ok bool)
	Set(hash, query string)
}

// Hash returns the hex-encoded SHA-256 of a query, the form the protocol
// sends.
func Hash(query string) string {
	sum := sha256.Sum256([]byte(query))
	return hex.EncodeToString(sum[:])
}

// Resolve applies the persistedQuery extension to req, filling in req.Query
// from the cache or registering the query it carries.
//
// It returns nil when the request may proceed, and a response to send when it
// may not: an unknown hash, an unsupported protocol version, or query text
// that does not hash to the hash given. Registration is verified rather than
// trusted, because a server that stored whatever text arrived alongside a
// hash would let one client choose what every later client's hash executes.
func Resolve(c Cache, req *graphql.Request) *graphql.Response {
	ext, ok := req.Extensions["persistedQuery"]
	if !ok {
		return nil
	}
	fields, ok := ext.(map[string]any)
	if !ok {
		return errorResponse(CodeNotSupported, "PersistedQueryNotSupported")
	}
	// JSON numbers decode as float64; anything but version 1 is a protocol
	// this server does not speak.
	if v, ok := fields["version"].(float64); !ok || v != 1 {
		return errorResponse(CodeNotSupported, "PersistedQueryNotSupported")
	}
	hash, _ := fields["sha256Hash"].(string)
	if hash == "" {
		return errorResponse(CodeNotSupported, "PersistedQueryNotSupported")
	}

	if req.Query == "" {
		query, found := c.Get(hash)
		if !found {
			return errorResponse(CodeNotFound, "PersistedQueryNotFound")
		}
		req.Query = query
		return nil
	}

	if Hash(req.Query) != hash {
		return errorResponse("", "provided sha does not match query")
	}
	c.Set(hash, req.Query)
	return nil
}

func errorResponse(code, message string) *graphql.Response {
	err := graphql.Errorf("%s", message)
	if code != "" {
		err = err.WithCode(code)
	}
	return &graphql.Response{Errors: []*graphql.Error{err}}
}

// NewCache returns an in-memory LRU holding at most size queries. A zero or
// negative size makes it unbounded, which is only safe when a trusted build
// step registers every hash: otherwise any client can grow it without limit.
func NewCache(size int) Cache {
	return &lru{size: size, index: make(map[string]*list.Element)}
}

type entry struct {
	hash  string
	query string
}

type lru struct {
	mu    sync.Mutex
	size  int
	order list.List
	index map[string]*list.Element
}

func (c *lru) Get(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[hash]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*entry).query, true
}

func (c *lru) Set(hash, query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[hash]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.index[hash] = c.order.PushFront(&entry{hash: hash, query: query})
	if c.size > 0 {
		for c.order.Len() > c.size {
			oldest := c.order.Back()
			c.order.Remove(oldest)
			delete(c.index, oldest.Value.(*entry).hash)
		}
	}
}
