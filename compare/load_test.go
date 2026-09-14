package compare_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The HTTP benchmarks in http_test.go send one request at a time, which
// measures the mean and hides what happens when the server is saturated.
// Allocation drives GC pressure, and GC pressure shows in the tail, so the
// allocation gap should tell here if it tells anywhere.
//
// Per-request latency percentiles are deliberately not reported. The
// monotonic clock on the machine these were written on has a granularity of
// roughly 522us -- 99 999 of 100 000 back-to-back time.Since calls return
// exactly zero -- so a request served in 70us cannot be timed individually at
// all, and the faster engine ends up with more unmeasurable samples than the
// slower one. Throughput over many requests is measured across thousands of
// clock ticks and is sound; percentiles need a platform with a finer clock.

// loadQuery is small enough that the engine, not the payload, decides the
// cost, and large enough to allocate.
const loadQuery = `{ entity000s(first: 20) {
	totalCount
	edges { cursor node { id name score } }
} }`

func benchHTTPParallel(b *testing.B, newServer func(testing.TB) (*httptest.Server, *http.Client)) {
	srv, c := newServer(b)
	// Without a pool the client opens a fresh connection per request and
	// exhausts the ephemeral port range under load; on Windows that surfaces
	// as "only one usage of each socket address".
	c.Transport = pooledTransport(256)
	postGraphQL(b, c, srv.URL, loadQuery)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			postGraphQL(b, c, srv.URL, loadQuery)
		}
	})
}

// BenchmarkParallelHTTPGraphQLGo and its gqlgen twin saturate the server with
// GOMAXPROCS clients rather than one.
func BenchmarkParallelHTTPGraphQLGo(b *testing.B) { benchHTTPParallel(b, newGraphQLGoServer) }
func BenchmarkParallelHTTPGqlgen(b *testing.B)    { benchHTTPParallel(b, newGqlgenServer) }

// throughput drives total requests across concurrency workers and returns the
// sustained rate.
func throughput(tb testing.TB, srv *httptest.Server, c *http.Client, concurrency, total int) float64 {
	tb.Helper()

	var wg sync.WaitGroup
	work := make(chan struct{})

	start := time.Now()
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				postGraphQL(tb, c, srv.URL, loadQuery)
			}
		}()
	}
	for range total {
		work <- struct{}{}
	}
	close(work)
	wg.Wait()

	return float64(total) / time.Since(start).Seconds()
}

// TestLoadThroughput reports sustained request rate for both engines under
// the same concurrency. It asserts only that every request succeeded; an
// absolute rate threshold would be a machine-specific flake.
func TestLoadThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("load test takes a few seconds")
	}
	const (
		concurrency = 32
		total       = 20000
	)

	for _, engine := range []struct {
		name      string
		newServer func(testing.TB) (*httptest.Server, *http.Client)
	}{
		{"graphql-go", newGraphQLGoServer},
		{"gqlgen", newGqlgenServer},
	} {
		t.Run(engine.name, func(t *testing.T) {
			srv, c := engine.newServer(t)
			c.Transport = pooledTransport(concurrency)

			rate := throughput(t, srv, c, concurrency, total)
			t.Logf("%s: %.0f req/s (%d requests, concurrency %d)",
				engine.name, rate, total, concurrency)
		})
	}
}

// pooledTransport keeps connections alive and caps how many exist at once.
// Without the cap the client opens a fresh connection per request and
// exhausts the ephemeral port range; on Windows that surfaces as "only one
// usage of each socket address".
func pooledTransport(n int) *http.Transport {
	return &http.Transport{
		MaxIdleConns:        n,
		MaxIdleConnsPerHost: n,
		MaxConnsPerHost:     n,
	}
}
