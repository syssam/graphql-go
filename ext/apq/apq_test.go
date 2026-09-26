package apq_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
)

const query = `{ ping }`

func ext(hash string, version float64) map[string]any {
	return map[string]any{
		"persistedQuery": map[string]any{"version": version, "sha256Hash": hash},
	}
}

func message(t *testing.T, resp *graphql.Response) string {
	t.Helper()
	if resp == nil {
		t.Fatal("wanted a response")
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("wanted one error, got %d", len(resp.Errors))
	}
	return resp.Errors[0].Message
}

func TestRegisterThenLookup(t *testing.T) {
	c := apq.NewCache(10)
	hash := apq.Hash(query)

	// First request carries both, which registers.
	reg := &graphql.Request{Query: query, Extensions: ext(hash, 1)}
	if resp := apq.Resolve(c, reg); resp != nil {
		t.Fatalf("registration failed: %s", message(t, resp))
	}

	// Later requests carry only the hash.
	lookup := &graphql.Request{Extensions: ext(hash, 1)}
	if resp := apq.Resolve(c, lookup); resp != nil {
		t.Fatalf("lookup failed: %s", message(t, resp))
	}
	if lookup.Query != query {
		t.Fatalf("query = %q, want %q", lookup.Query, query)
	}
}

func TestUnknownHash(t *testing.T) {
	c := apq.NewCache(10)
	req := &graphql.Request{Extensions: ext(apq.Hash(query), 1)}
	resp := apq.Resolve(c, req)
	if got := message(t, resp); got != "PersistedQueryNotFound" {
		t.Fatalf("message = %q", got)
	}
	if got := resp.Errors[0].Extensions["code"]; got != apq.CodeNotFound {
		t.Fatalf("code = %v", got)
	}
	// The response must be a request error so transports give it the status
	// their media type calls for.
	if !resp.HasRequestErrors() {
		t.Fatal("PersistedQueryNotFound should be a request error")
	}
}

// TestMismatchedHashIsRejected is the cache-poisoning guard: a server that
// stored whatever text arrived with a hash would let one client choose what
// every later client's hash executes.
func TestMismatchedHashIsRejected(t *testing.T) {
	c := apq.NewCache(10)
	hash := apq.Hash(query)

	evil := &graphql.Request{Query: `mutation { drop }`, Extensions: ext(hash, 1)}
	resp := apq.Resolve(c, evil)
	if got := message(t, resp); !strings.Contains(got, "does not match") {
		t.Fatalf("message = %q", got)
	}
	if _, ok := c.Get(hash); ok {
		t.Fatal("a query whose hash did not match was stored anyway")
	}
}

func TestUnsupportedVersions(t *testing.T) {
	c := apq.NewCache(10)
	hash := apq.Hash(query)

	tests := []struct {
		name string
		ext  map[string]any
	}{
		{"version 2", ext(hash, 2)},
		{"missing hash", ext("", 1)},
		{"payload is not an object", map[string]any{"persistedQuery": "nope"}},
		{"missing version", map[string]any{"persistedQuery": map[string]any{"sha256Hash": hash}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &graphql.Request{Extensions: tc.ext}
			if got := message(t, apq.Resolve(c, req)); got != "PersistedQueryNotSupported" {
				t.Fatalf("message = %q", got)
			}
		})
	}
}

func TestNoExtensionIsUntouched(t *testing.T) {
	c := apq.NewCache(10)
	req := &graphql.Request{Query: query}
	if resp := apq.Resolve(c, req); resp != nil {
		t.Fatalf("an ordinary request should pass through: %s", message(t, resp))
	}
	if req.Query != query {
		t.Fatalf("query was modified: %q", req.Query)
	}
	req = &graphql.Request{Query: query, Extensions: map[string]any{"tracing": true}}
	if resp := apq.Resolve(c, req); resp != nil {
		t.Fatalf("an unrelated extension should pass through: %s", message(t, resp))
	}
}

func TestCacheEviction(t *testing.T) {
	c := apq.NewCache(2)
	c.Set("a", "{ a }")
	c.Set("b", "{ b }")
	// Touching a makes b the oldest.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be cached")
	}
	c.Set("c", "{ c }")

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as least recently used")
	}
	for _, hash := range []string{"a", "c"} {
		if _, ok := c.Get(hash); !ok {
			t.Fatalf("%s should still be cached", hash)
		}
	}
}

func TestCacheSetIsIdempotent(t *testing.T) {
	c := apq.NewCache(2)
	c.Set("a", "{ a }")
	c.Set("a", "{ a }")
	c.Set("b", "{ b }")
	if _, ok := c.Get("a"); !ok {
		t.Fatal("re-setting a key should not have consumed a second slot")
	}
}

func TestUnboundedCache(t *testing.T) {
	c := apq.NewCache(0)
	for _, h := range []string{"a", "b", "c", "d"} {
		c.Set(h, "{ "+h+" }")
	}
	for _, h := range []string{"a", "b", "c", "d"} {
		if _, ok := c.Get(h); !ok {
			t.Fatalf("%s should be cached when the size is unbounded", h)
		}
	}
}

func TestHashIsHexSHA256(t *testing.T) {
	// Pinned against `printf '{ ping }' | sha256sum`, because this is the
	// value a client computes independently: a change to the encoding here
	// would break every client rather than fail a test.
	const want = "6cd3bf61757c6bee6e943d50a381a002447236bf3f15d3730400b931e9cf323f"
	if got := apq.Hash("{ ping }"); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if apq.Hash("a") == apq.Hash("b") {
		t.Fatal("different queries hashed the same")
	}
}

// Any client can register any query it can hash, so an entry count alone lets
// 1000 entries of 1 MiB each hold a gigabyte of attacker-chosen text. The
// cache is bounded by the text it holds as well.
func TestCacheIsBoundedByBytes(t *testing.T) {
	c := apq.NewCache(1000, apq.WithMaxBytes(10))
	c.Set("a", "12345")
	c.Set("b", "12345")
	c.Set("c", "12345") // 15 bytes: a must go
	if _, ok := c.Get("a"); ok {
		t.Fatal("the least recently used entry survived going over the byte budget")
	}
	for _, h := range []string{"b", "c"} {
		if _, ok := c.Get(h); !ok {
			t.Fatalf("%s was evicted, but b and c fit in the budget", h)
		}
	}

	c.Set("huge", "12345678901")
	if _, ok := c.Get("huge"); ok {
		t.Fatal("a query larger than the whole budget was stored")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("storing nothing still evicted what was there")
	}
}

func TestCacheHasADefaultByteBudget(t *testing.T) {
	c := apq.NewCache(0)
	big := strings.Repeat("x", 1<<20)
	for i := range 64 {
		c.Set(strconv.Itoa(i), big)
	}
	if _, ok := c.Get("0"); ok {
		t.Fatal("64 MiB of query text was held by a cache with no explicit byte limit")
	}
	if _, ok := c.Get("63"); !ok {
		t.Fatal("the newest entry was evicted")
	}

	unbounded := apq.NewCache(0, apq.WithMaxBytes(0))
	for i := range 64 {
		unbounded.Set(strconv.Itoa(i), big)
	}
	if _, ok := unbounded.Get("0"); !ok {
		t.Fatal("WithMaxBytes(0) still evicted")
	}
}

// Hex digits are case-insensitive, so a client that sends its digest in upper
// case is sending the same hash. Compared as written, it failed verification
// against the very text it hashed, and a lookup missed a cache the other
// spelling had warmed.
func TestHashCaseIsIgnored(t *testing.T) {
	lower := apq.Hash(query)
	upper := strings.ToUpper(lower)

	c := apq.NewCache(10)
	if resp := apq.Resolve(c, &graphql.Request{Query: query, Extensions: ext(upper, 1)}); resp != nil {
		t.Fatalf("upper-case registration refused: %s", message(t, resp))
	}
	for _, h := range []string{lower, upper} {
		req := &graphql.Request{Extensions: ext(h, 1)}
		if resp := apq.Resolve(c, req); resp != nil {
			t.Fatalf("lookup by %s: %s", h, message(t, resp))
		}
		if req.Query != query {
			t.Fatalf("lookup by %s: query = %q", h, req.Query)
		}
	}
}
