package graphql

import (
	"context"
	"sync"
	"testing"
)

func TestOperationContextGetOrSet(t *testing.T) {
	oc := &OperationContext{}

	if v, loaded := oc.GetOrSet("k", 1); loaded || v != 1 {
		t.Fatalf("first GetOrSet = (%v, %v), want (1, false)", v, loaded)
	}
	if v, loaded := oc.GetOrSet("k", 2); !loaded || v != 1 {
		t.Fatalf("second GetOrSet = (%v, %v), want (1, true)", v, loaded)
	}
	if v, ok := oc.Get("k"); !ok || v != 1 {
		t.Fatalf("Get = (%v, %v), want (1, true)", v, ok)
	}
}

// TestOperationContextGetOrSetIsAtomic is the property request-scoped
// extensions depend on: when many resolvers initialise the same key at once,
// exactly one wins and every caller observes that same value.
func TestOperationContextGetOrSetIsAtomic(t *testing.T) {
	oc := &OperationContext{}

	const n = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	actuals := make([]any, n)
	start := make(chan struct{})

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, loaded := oc.GetOrSet("k", i)
			actuals[i] = v
			if !loaded {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	for i, v := range actuals {
		if v != actuals[0] {
			t.Fatalf("goroutine %d observed %v, want %v for every caller", i, v, actuals[0])
		}
	}
}

// TestPathFromInsideResolver pins the public guarantee this change must not
// touch: a resolver field can still read its own path out of the context.
func TestPathFromInsideResolver(t *testing.T) {
	var got Path
	s, err := NewSchema(SDL(`type Query { deep: Inner } type Inner { v: String }`),
		Object[Root]("Query", Resolve("deep", func(ctx context.Context, _ Root) (*inner, error) {
			got = PathFrom(ctx)
			return &inner{}, nil
		})),
		Object[inner]("Inner", Field("v", func(*inner) string { return "x" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	run(t, NewExecutor(s), `{ deep { v } }`, "")
	if len(got) != 1 || got[0].Key != "deep" {
		t.Fatalf("PathFrom = %v, want [deep]", got)
	}
}

type inner struct{}
