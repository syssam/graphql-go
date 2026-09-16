package graphql

import (
	"encoding/json"
	"sync"
	"testing"
)

// verifyFanChain decodes a fanQuery(depth) response and walks the root ->
// next -> next -> ... chain, requiring a real id at every level through
// depth. This is the check FINDING-20 said was missing: fanObject used to be
// Field-bound and always returned nil, so writeValue (exec_object.go:154-155)
// wrote {"root": null} before ever touching the shared selectionSet the race
// tests exist to cover, and neither test noticed because neither looked at
// the data. A resolver that stops recursing early, or an abstract-dispatch
// bug that resolves the wrong type partway down, fails here instead of
// silently passing as "no errors".
func verifyFanChain(t *testing.T, data []byte, depth int) {
	t.Helper()
	// Response.Data (request.go) is already the operation's data object, not
	// the {"data": ...} envelope — WriteTo adds that wrapper only when
	// serializing the full response.
	var env struct {
		Root map[string]any `json:"root"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode response data: %v (data = %s)", err, data)
	}
	node := env.Root
	for level := 0; level <= depth; level++ {
		if node == nil {
			t.Fatalf("chain stopped at level %d, want it to reach level %d: the fixture did not recurse", level, depth)
		}
		id, _ := node["id"].(string)
		if id == "" {
			t.Fatalf("level %d: id is empty or missing, want a real value (node = %v)", level, node)
		}
		if level == depth {
			break
		}
		next, _ := node["next"].(map[string]any)
		node = next
	}
}

// TestFanOutFieldConcurrencyFires isolates writeFieldsConcurrent's
// precondition from the multi-request races below: a single request, driven
// from one goroutine with no sibling requests running, must still overlap
// "id" and "next" internally. Without this, the concurrency witness in the
// tests below could pass on inter-request parallelism alone (sixteen
// requests happening to overlap each other) while writeFieldsConcurrent
// never fires within any one of them — which is exactly the gap FINDING-20
// identified: "concurrent sibling resolvers" requires intra-request
// scheduling, not just many requests in flight at once.
func TestFanOutFieldConcurrencyFires(t *testing.T) {
	_, e := newFanExecutor(t)
	const depth = 4
	fanResetConcurrencyWitness()
	resp := e.Execute(t.Context(), &Request{Query: fanQuery(depth)})
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	verifyFanChain(t, resp.Data, depth)
	if got := fanMaxInFlight.Load(); got < 2 {
		t.Fatalf("a single request observed max concurrent resolver calls = %d, want >= 2: writeFieldsConcurrent did not fire within one request", got)
	}
}

// TestFanOutSharedSelectionSetsRace covers what memoization introduced: one
// selection set is now reached by many paths, and concurrent sibling
// resolvers read it at once. It is only meaningful under -race.
//
// fanObject binds "id" and "next" through Resolve (schedulable) and "next"
// returns a real node every level (plan_fanout_test.go), so this actually
// walks the full depth-4 chain through writeObject -> writeFieldsConcurrent
// -> writeValue -> concreteValue -> writeObject at every level, rather than
// hitting the v == nil short-circuit in writeValue on the first field. The
// concurrency witness (fanMaxInFlight) confirms goroutines were genuinely
// overlapping inside that shared structure, not just running one after
// another fast enough that -race never observed two touching it at once.
func TestFanOutSharedSelectionSetsRace(t *testing.T) {
	s, e := newFanExecutor(t)
	const depth = 4
	query := fanQuery(depth)
	fanResetConcurrencyWitness()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.Execute(t.Context(), &Request{Query: query})
			if len(resp.Errors) != 0 {
				t.Errorf("unexpected errors: %v", resp.Errors)
				return
			}
			verifyFanChain(t, resp.Data, depth)
		}()
	}
	wg.Wait()
	_ = s

	// directSchedulable requires >=2 schedulable fields per selection
	// (plan.go), which "id" and "next" both being Resolve-bound satisfies;
	// this checks that requirement actually produced overlapping resolver
	// calls at runtime, not just a plan that was eligible for it.
	if got := fanMaxInFlight.Load(); got < 2 {
		t.Fatalf("max concurrent resolver calls observed = %d, want >= 2: writeFieldsConcurrent never overlapped two calls into the shared selection", got)
	}
}

// TestFanOutConcurrentCompileRace drives the compile path itself rather than
// the cached plan: sixteen goroutines race to be the one that compiles.
func TestFanOutConcurrentCompileRace(t *testing.T) {
	const depth = 3
	fanResetConcurrencyWitness()
	for i := 0; i < 8; i++ {
		s, e := newFanExecutor(t)
		query := fanQuery(depth)
		var wg sync.WaitGroup
		for j := 0; j < 16; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp := e.Execute(t.Context(), &Request{Query: query})
				if len(resp.Errors) != 0 {
					t.Errorf("unexpected errors: %v", resp.Errors)
					return
				}
				verifyFanChain(t, resp.Data, depth)
			}()
		}
		wg.Wait()
		_ = s
	}

	if got := fanMaxInFlight.Load(); got < 2 {
		t.Fatalf("max concurrent resolver calls observed = %d, want >= 2 across 8 rounds of 16 goroutines", got)
	}
}
