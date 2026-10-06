package graphql

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestStatsCountsPlanCache pins what the plan cache figures mean: one miss
// for the first run of an operation, a hit for every run after, and the
// cache holding the one document by its query text and what its plan retains.
func TestStatsCountsPlanCache(t *testing.T) {
	_, e := newFixtureExecutor(t)
	const query = `{ me { id } }`
	if s := e.Stats(); s.PlanCacheHits != 0 || s.PlanCacheMisses != 0 || s.PlanCacheEntries != 0 {
		t.Fatalf("fresh executor stats = %+v, want zeroes", s)
	}
	for range 3 {
		run(t, e, query, "")
	}
	s := e.Stats()
	if s.PlanCacheMisses != 1 || s.PlanCacheHits != 2 {
		t.Fatalf("hits = %d, misses = %d after three runs, want 2 and 1", s.PlanCacheHits, s.PlanCacheMisses)
	}
	want := cachedBytes(e.cache)
	if s.PlanCacheEntries != 1 || s.PlanCacheBytes != want || want <= int64(len(query)) {
		t.Fatalf("entries = %d, bytes = %d, want 1 and %d, the %d of query text and the plan", s.PlanCacheEntries, s.PlanCacheBytes, want, len(query))
	}
}

// TestStatsIgnoresRejectedAndKindLookups: a request that never reaches a plan
// is neither a hit nor a miss, and OperationKind — which every streaming
// transport calls before Execute — must not count the same request twice.
func TestStatsIgnoresRejectedAndKindLookups(t *testing.T) {
	_, e := newFixtureExecutor(t)
	run(t, e, `{ nope }`, "")
	if _, err := e.OperationKind(`{ me { id } }`, ""); err != nil {
		t.Fatal(err)
	}
	run(t, e, `{ me { id } }`, "")
	s := e.Stats()
	if s.PlanCacheHits != 0 || s.PlanCacheMisses != 1 {
		t.Fatalf("hits = %d, misses = %d, want 0 and 1", s.PlanCacheHits, s.PlanCacheMisses)
	}
}

// TestStatsReportsConcurrency: the limit is the configured semaphore and the
// in-use count reflects resolvers holding a slot right now.
func TestStatsReportsConcurrency(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	s, err := NewSchema(SDL(`type Query { a: Int! b: Int! }`),
		Query(
			Resolve("a", func(context.Context, Root) (int, error) { entered <- struct{}{}; <-release; return 1, nil }),
			Resolve("b", func(context.Context, Root) (int, error) { entered <- struct{}{}; <-release; return 2, nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, WithMaxConcurrency(3))
	if got := e.Stats(); got.ConcurrencyLimit != 3 || got.ConcurrencyInUse != 0 {
		t.Fatalf("idle stats = %+v, want limit 3 and nothing in use", got)
	}

	var wg sync.WaitGroup
	wg.Go(func() { run(t, e, `{ a b }`, "") })
	<-entered
	<-entered
	if got := e.Stats().ConcurrencyInUse; got != 2 {
		t.Fatalf("in use = %d with two resolvers parked, want 2", got)
	}
	close(release)
	wg.Wait()
	if got := e.Stats().ConcurrencyInUse; got != 0 {
		t.Fatalf("in use = %d after the request finished, want 0", got)
	}
}

func TestStatsWithoutConcurrency(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxConcurrency(0))
	if got := e.Stats(); got.ConcurrencyLimit != 0 || got.ConcurrencyInUse != 0 {
		t.Fatalf("stats = %+v with concurrency disabled, want zeroes", got)
	}
}

// TestStatsCountsSubscriptionOnce pins the unit for a subscription: one plan
// lookup when it opens, however many events it delivers, since no event
// looks a plan up again.
func TestStatsCountsSubscriptionOnce(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := e.Subscribe(ctx, &Request{Query: `subscription { counter }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		for i := range 3 {
			src.counter <- i
		}
		close(src.counter)
	}()
	for range 3 {
		nextResponse(t, out).Release()
	}
	expectClosed(t, out)
	if s := e.Stats(); s.PlanCacheMisses != 1 || s.PlanCacheHits != 0 {
		t.Fatalf("hits = %d, misses = %d for one subscription of three events, want 0 and 1", s.PlanCacheHits, s.PlanCacheMisses)
	}
}

// With none of WithMaxDepth, WithMaxComplexity and a query cost limit set, a
// single request may ask for any amount of work. That is the documented
// default, since the right numbers are a schema's own, and it has to be
// visible: said once in the log when such an executor is built, and reported
// in Stats for a dashboard, so that the first person on call does not learn
// it from the request that used it.
func TestAnExecutorWithNoRequestLimitSaysSo(t *testing.T) {
	s, err := NewSchema(SDL(fixtureSDL), newFixture().options()...)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		opts      []ExecutorOption
		unbounded bool
	}{
		"nothing set":            {nil, true},
		"a cost model, no limit": {[]ExecutorOption{WithQueryCost(QueryCost{DefaultListSize: 20})}, true},
		"depth":                  {[]ExecutorOption{WithMaxDepth(12)}, false},
		"complexity":             {[]ExecutorOption{WithMaxComplexity(500)}, false},
		"a cost limit":           {[]ExecutorOption{WithQueryCost(QueryCost{Max: 1000})}, false},
	} {
		t.Run(name, func(t *testing.T) {
			unboundedSaid.Store(false)
			logged := captureLog(t)
			e := NewExecutor(s, tc.opts...)
			NewExecutor(s, tc.opts...)
			if got := e.Stats().RequestsUnbounded; got != tc.unbounded {
				t.Errorf("Stats().RequestsUnbounded = %t, want %t", got, tc.unbounded)
			}
			want := 0
			if tc.unbounded {
				want = 1
			}
			if got := strings.Count(logged.String(), "no depth, complexity or cost limit"); got != want {
				t.Errorf("said %d time(s) for two executors, want %d:\n%s", got, want, logged)
			}
		})
	}
}
