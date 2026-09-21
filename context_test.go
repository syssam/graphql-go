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

// A resolver's context is a type of this package's own rather than a
// context.WithValue wrapper, so the FieldContext and the context carrying it
// are one allocation. Its Value must therefore delegate every other key: the
// OperationContext that every DataLoader and request-scoped extension reaches
// through, and anything the caller put on the context before Execute. Only
// one subscription test noticed when delegation was broken on purpose, which
// is why this one asks on the query path directly.
func TestResolverContextDelegatesEveryOtherKey(t *testing.T) {
	var (
		oc     *OperationContext
		fc     *FieldContext
		caller any
	)
	s, err := NewSchema(SDL(`type Query { deep: Inner } type Inner { v: String }`),
		Object[Root]("Query", Resolve("deep", func(ctx context.Context, _ Root) (*inner, error) {
			oc, fc, caller = OperationFrom(ctx), FieldFrom(ctx), ctx.Value(callerCtxKey{})
			return &inner{}, nil
		})),
		Object[inner]("Inner", Field("v", func(*inner) string { return "x" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), callerCtxKey{}, "caller")
	if resp := NewExecutor(s).Execute(ctx, &Request{Query: `{ deep { v } }`}); len(resp.Errors) > 0 {
		t.Fatalf("execute: %v", resp.Errors[0])
	}

	if oc == nil {
		t.Error("OperationFrom found nothing inside a resolver")
	}
	if fc == nil || fc.Field.Name != "deep" {
		t.Errorf("FieldFrom = %+v, want the deep field", fc)
	}
	if caller != "caller" {
		t.Errorf("caller's own context value = %v, want \"caller\"", caller)
	}
}

type callerCtxKey struct{}
