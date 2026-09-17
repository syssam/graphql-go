package graphql

import (
	"context"
	"sync"
	"testing"
)

// TestStatsCountsPlanCache pins what the plan cache figures mean: one miss
// for the first run of an operation, a hit for every run after, and the
// cache holding the one document by its query text.
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
	if s.PlanCacheEntries != 1 || s.PlanCacheBytes != int64(len(query)) {
		t.Fatalf("entries = %d, bytes = %d, want 1 and %d", s.PlanCacheEntries, s.PlanCacheBytes, len(query))
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
