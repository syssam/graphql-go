package loader_test

import (
	"context"
	"errors"
	"testing"

	"github.com/syssam/graphql-go/loader"
)

// Two things BatchFunc and MappedBatchFunc promise in their godoc, neither
// named by a test.
//
// "Neither kind of error is cached." A cached failure would mean one transient
// database blip poisons every later Load of that key for the rest of the
// request -- and since the cache is per request, the client sees a partial
// response whose holes have nothing to do with the data. Caching a failure is
// also the easy accident: the success path caches, and sharing that line is
// how it happens.
//
// "Keys are unique." The batch function is written against that promise --
// `WHERE id IN (...)` with a duplicate is wasted work at best, and a caller
// indexing the result by position would be wrong. Two resolvers loading the
// same key in one wave is the ordinary case, not a corner.

func TestABatchErrorIsNotCached(t *testing.T) {
	var calls int
	fail := errors.New("database unreachable")
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		calls++
		if calls == 1 {
			return nil, fail
		}
		out := make(map[int]int, len(keys))
		for _, k := range keys {
			out[k] = k * 10
		}
		return out, nil
	})

	ctx := loader.WithScope(context.Background())
	if _, err := ld.Load(ctx, 1); !errors.Is(err, fail) {
		t.Fatalf("first Load err = %v, want the batch error", err)
	}
	v, err := ld.Load(ctx, 1)
	if err != nil {
		t.Fatalf("second Load err = %v; the failure was cached, so a transient "+
			"outage poisons this key for the rest of the request", err)
	}
	if v != 10 {
		t.Errorf("second Load = %d, want 10", v)
	}
	if calls != 2 {
		t.Errorf("batch ran %d times, want 2: the retry must reach the batch function", calls)
	}
}

func TestAPerKeyErrorIsNotCached(t *testing.T) {
	var calls int
	fail := errors.New("row locked")
	ld := loader.NewMapped(func(_ context.Context, keys []int) (map[int]int, map[int]error, error) {
		calls++
		if calls == 1 {
			return nil, map[int]error{1: fail}, nil
		}
		return map[int]int{1: 10}, nil, nil
	})

	ctx := loader.WithScope(context.Background())
	if _, err := ld.Load(ctx, 1); !errors.Is(err, fail) {
		t.Fatalf("first Load err = %v, want the per-key error", err)
	}
	v, err := ld.Load(ctx, 1)
	if err != nil {
		t.Fatalf("second Load err = %v; a per-key failure was cached", err)
	}
	if v != 10 {
		t.Errorf("second Load = %d, want 10", v)
	}
	if calls != 2 {
		t.Errorf("batch ran %d times, want 2", calls)
	}
}

// A *successful* result is cached, which is the whole point of the loader and
// the control that says the two tests above are measuring a failure not being
// cached rather than a cache that never works.
func TestASuccessIsCached(t *testing.T) {
	var calls int
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		calls++
		return map[int]int{1: 10}, nil
	})
	ctx := loader.WithScope(context.Background())
	for range 3 {
		if v, err := ld.Load(ctx, 1); err != nil || v != 10 {
			t.Fatalf("Load = (%d, %v)", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("batch ran %d times for three Loads of one key, want 1", calls)
	}
}

// LoadMany with the same key twice, and Load of a key already pending: the
// batch function sees each key once.
//
// Removing the dedup is caught, but by a hang rather than by this assertion --
// a second waiter on a key the queue only carries once never receives a
// result. Run a break here with a short -timeout. What the assertion adds is
// the other direction: a change that keeps the waiter bookkeeping correct but
// lets duplicates into the queue would pass every existing test and quietly
// double the work a batch does.
func TestTheBatchFunctionSeesEachKeyOnce(t *testing.T) {
	var seen [][]int
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		seen = append(seen, append([]int(nil), keys...))
		out := make(map[int]int, len(keys))
		for _, k := range keys {
			out[k] = k * 10
		}
		return out, nil
	})

	got, err := ld.LoadMany(context.Background(), []int{1, 2, 1, 2, 1})
	if err != nil {
		t.Fatalf("LoadMany: %v", err)
	}
	// The result still matches the keys asked for, duplicates included.
	if want := []int{10, 20, 10, 20, 10}; len(got) != len(want) {
		t.Fatalf("LoadMany returned %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("LoadMany returned %v, want %v", got, want)
			}
		}
	}
	if len(seen) != 1 {
		t.Fatalf("the batch ran %d times for one LoadMany", len(seen))
	}
	counts := map[int]int{}
	for _, k := range seen[0] {
		counts[k]++
	}
	for k, n := range counts {
		if n != 1 {
			t.Errorf("the batch saw key %d %d times; BatchFunc's godoc promises unique keys, "+
				"and a caller writing WHERE id IN (...) relies on it", k, n)
		}
	}
}

// LoadMany of nothing must not reach the batch function at all: an empty
// IN (...) is a syntax error in most dialects, and a caller passing an empty
// slice is ordinary -- it is what an empty parent list produces.
func TestLoadManyOfNoKeysDoesNotCallTheBatch(t *testing.T) {
	var calls int
	ld := loader.New(func(_ context.Context, keys []int) (map[int]int, error) {
		calls++
		return nil, nil
	})
	got, err := ld.LoadMany(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMany(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadMany(nil) = %v, want empty", got)
	}
	if calls != 0 {
		t.Errorf("the batch ran %d times for no keys", calls)
	}
}
