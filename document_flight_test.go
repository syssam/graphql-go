package graphql

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// These tests hold the first parse open with testHookParseDocument and use
// synctest.Wait to know every other request has reached whatever it blocks on
// before letting the parse finish. That makes "concurrent" exact rather than
// a matter of scheduling luck.

func withParseHook(t *testing.T, hook func()) {
	t.Helper()
	testHookParseDocument = hook
	t.Cleanup(func() { testHookParseDocument = nil })
}

// TestDocumentParsedOncePerConcurrentMiss is the stampede: many requests for
// the same uncached text arrive together, and only one of them parses and
// validates it. Without dedup each parses, which for a 100 KB query measured
// 64 concurrent requests at 19 times the wall time of one.
func TestDocumentParsedOncePerConcurrentMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newFixtureExecutor(t)
		const query = `{ me { id name } }`

		var parses atomic.Int32
		release := make(chan struct{})
		withParseHook(t, func() {
			parses.Add(1)
			<-release
		})

		const n = 8
		entries := make([]*docEntry, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				entry, errs := e.document(query)
				if errs != nil {
					t.Errorf("request %d: %v", i, errs[0])
				}
				entries[i] = entry
			})
		}
		synctest.Wait() // every request is parked: one in the hook, the rest waiting on it
		close(release)
		wg.Wait()

		if got := parses.Load(); got != 1 {
			t.Fatalf("parsed %d times for %d concurrent requests, want 1", got, n)
		}
		for i, entry := range entries {
			if entry == nil || entry != entries[0] {
				t.Fatalf("request %d got entry %p, want the shared %p", i, entry, entries[0])
			}
		}
	})
}

// TestDocumentFlightErrorsAreNotShared pins that a failed parse shared across
// waiting requests hands each its own errors: a presenter annotating one
// request's error must not change another's.
func TestDocumentFlightErrorsAreNotShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newFixtureExecutor(t)
		const query = `{ me { nope } }`

		release := make(chan struct{})
		withParseHook(t, func() { <-release })

		var errsA, errsB []*Error
		var wg sync.WaitGroup
		wg.Go(func() { _, errsA = e.document(query) })
		synctest.Wait()
		wg.Go(func() { _, errsB = e.document(query) })
		synctest.Wait()
		close(release)
		wg.Wait()

		if len(errsA) == 0 || len(errsB) == 0 {
			t.Fatalf("want validation errors for both, got %v and %v", errsA, errsB)
		}
		if errsA[0] == errsB[0] {
			t.Fatal("two requests share one *Error; a presenter mutating it would race")
		}
		errsB[0].Message = "changed"
		if errsA[0].Message == "changed" {
			t.Fatal("changing one request's error changed the other's")
		}
	})
}

// TestDocumentFailuresAreNotCached pins the half of #5 that was declined: an
// invalid query is parsed again on the next request, so distinct invalid
// queries cannot evict valid entries from the cache.
func TestDocumentFailuresAreNotCached(t *testing.T) {
	_, e := newFixtureExecutor(t)
	var parses atomic.Int32
	withParseHook(t, func() { parses.Add(1) })

	for range 2 {
		if _, errs := e.document(`{ me { nope } }`); errs == nil {
			t.Fatal("want validation errors")
		}
	}
	if got := parses.Load(); got != 2 {
		t.Fatalf("parsed %d times across two sequential invalid requests, want 2", got)
	}
	if got := e.cache.len(); got != 0 {
		t.Fatalf("cache holds %d entries after only invalid queries, want 0", got)
	}
}

// TestDocumentFlightSurvivesLeaderPanic pins that a panic in the request doing
// the parse neither strands the requests waiting on it nor hands them a nil
// entry with no error, and that the text can be parsed again afterwards.
func TestDocumentFlightSurvivesLeaderPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, e := newFixtureExecutor(t)
		const query = `{ me { id } }`

		release := make(chan struct{})
		var calls atomic.Int32
		withParseHook(t, func() {
			if calls.Add(1) == 1 {
				<-release
				panic("parser exploded")
			}
		})

		var wg sync.WaitGroup
		wg.Go(func() {
			defer func() { _ = recover() }()
			e.document(query)
		})
		synctest.Wait()

		var entry *docEntry
		var errs []*Error
		wg.Go(func() { entry, errs = e.document(query) })
		synctest.Wait()
		close(release)
		wg.Wait()

		if entry != nil || len(errs) == 0 {
			t.Fatalf("a request waiting on a panicked parse got entry %v and errors %v, want an error", entry, errs)
		}
		if again, errs := e.document(query); again == nil || errs != nil {
			t.Fatalf("after the panic the text did not parse again: %v", errs)
		}
	})
}
