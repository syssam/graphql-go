package graphql

import (
	"sync"
	"testing"
)

// TestFanOutSharedSelectionSetsRace covers what memoization introduced: one
// selection set is now reached by many paths, and concurrent sibling
// resolvers read it at once. It is only meaningful under -race.
func TestFanOutSharedSelectionSetsRace(t *testing.T) {
	s, e := newFanExecutor(t)
	query := fanQuery(4)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.Execute(t.Context(), &Request{Query: query})
			if len(resp.Errors) != 0 {
				t.Errorf("unexpected errors: %v", resp.Errors)
			}
		}()
	}
	wg.Wait()
	_ = s
}

// TestFanOutConcurrentCompileRace drives the compile path itself rather than
// the cached plan: sixteen goroutines race to be the one that compiles.
func TestFanOutConcurrentCompileRace(t *testing.T) {
	for i := 0; i < 8; i++ {
		s, e := newFanExecutor(t)
		query := fanQuery(3)
		var wg sync.WaitGroup
		for j := 0; j < 16; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if resp := e.Execute(t.Context(), &Request{Query: query}); len(resp.Errors) != 0 {
					t.Errorf("unexpected errors: %v", resp.Errors)
				}
			}()
		}
		wg.Wait()
		_ = s
	}
}
