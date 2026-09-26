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
	"strings"
	"sync"

	"github.com/syssam/graphql-go"
)

// Error codes carried in extensions.code.
const (
	CodeNotFound     = "PERSISTED_QUERY_NOT_FOUND"
	CodeNotSupported = "PERSISTED_QUERY_NOT_SUPPORTED"
	// CodeNotInList is Apollo Router's spelling for a freeform document
	// refused by a safelist, so a client that already handles their router
	// handles this one.
	CodeNotInList = "PERSISTED_QUERY_NOT_IN_LIST"
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

// TrustedStore is a Cache that never learns: it answers from documents
// registered ahead of time and nothing else. Resolve treats one differently
// from an ordinary cache — query text is refused rather than registered, and
// a request carrying no hash at all is refused too, because a safelist that
// let freeform documents past would not bound anything.
//
// ext/trusted implements it.
type TrustedStore interface {
	Cache
	TrustedDocuments()
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
	_, trusted := c.(TrustedStore)
	ext, ok := req.Extensions["persistedQuery"]
	if !ok {
		if trusted {
			return errorResponse(CodeNotInList, "PersistedQueryNotInList")
		}
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

	// Hex is case-insensitive and Hash writes lower case, so an upper-case
	// digest from a client would otherwise miss a cache it had already warmed
	// and fail verification against the very text it hashed. A safelist's ids
	// are opaque (they may be build ids rather than digests), so they are
	// matched exactly as registered.
	if !trusted {
		hash = strings.ToLower(hash)
	}

	if req.Query == "" {
		query, found := c.Get(hash)
		if !found {
			return errorResponse(CodeNotFound, "PersistedQueryNotFound")
		}
		req.Query = query
		return nil
	}

	// A safelist bounds what the server runs, so text is refused however it
	// hashes: verifying it would only prove the client can hash.
	if trusted {
		return errorResponse(CodeNotInList, "PersistedQueryNotInList")
	}
	if Hash(req.Query) != hash {
		return errorResponse("", "provided sha does not match query")
	}
	c.Set(hash, req.Query)
	return nil
}

// IsRetryHandshake reports whether resp is the protocol asking the client to
// send the query text, rather than refusing the request.
//
// The distinction is a status code. Apollo Server answers both of these with
// 200 whatever media type was negotiated, because a client that sees 4xx here
// reports a failed request instead of retrying, and the retry is the whole
// protocol: every cold cache starts with one. CodeNotInList is deliberately
// absent -- a safelist refusal is a rejection, and retrying with the text is
// the thing it exists to forbid.
func IsRetryHandshake(resp *graphql.Response) bool {
	if resp == nil {
		return false
	}
	for _, e := range resp.Errors {
		switch e.Extensions["code"] {
		case CodeNotFound, CodeNotSupported:
			return true
		}
	}
	return false
}

func errorResponse(code, message string) *graphql.Response {
	err := graphql.Errorf("%s", message)
	if code != "" {
		err = err.WithCode(code)
	}
	return &graphql.Response{Errors: []*graphql.Error{err}}
}

// NewCache returns an in-memory LRU holding at most size queries and, by
// default, at most 16 MiB of query text. A zero or negative size removes the
// entry limit; WithMaxBytes(0) removes the byte limit. Removing both is only
// safe when a trusted build step registers every hash: otherwise any client
// can grow the cache without limit.
//
// The byte limit is the one that matters against a hostile client. Anyone can
// register any query they can hash, and with a 1 MiB request body an entry
// count of 1000 alone would let them park a gigabyte of text here.
func NewCache(size int, opts ...CacheOption) Cache {
	c := &lru{size: size, maxBytes: defaultMaxBytes, index: make(map[string]*list.Element)}
	for _, o := range opts {
		o(c)
	}
	return c
}

const defaultMaxBytes = 16 << 20

// CacheOption configures NewCache.
type CacheOption func(*lru)

// WithMaxBytes bounds the query text the cache holds, evicting the least
// recently used queries to stay within it. A query larger than the whole
// budget is not stored; the request still runs. Zero or negative means no
// byte limit. The default is 16 MiB.
func WithMaxBytes(n int64) CacheOption {
	return func(c *lru) { c.maxBytes = n }
}

type entry struct {
	hash  string
	query string
}

type lru struct {
	mu       sync.Mutex
	size     int
	maxBytes int64
	bytes    int64
	order    list.List
	index    map[string]*list.Element
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
	n := int64(len(query))
	if c.maxBytes > 0 && n > c.maxBytes {
		return
	}
	c.index[hash] = c.order.PushFront(&entry{hash: hash, query: query})
	c.bytes += n
	for (c.size > 0 && c.order.Len() > c.size) || (c.maxBytes > 0 && c.bytes > c.maxBytes) {
		oldest := c.order.Back()
		e := oldest.Value.(*entry)
		c.order.Remove(oldest)
		delete(c.index, e.hash)
		c.bytes -= int64(len(e.query))
	}
}
