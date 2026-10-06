package throttle_test

import (
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/throttle"
)

// spend runs items(first: 59), which costs 60 of a bucket of 100: a bucket
// answers 40 once and refuses the second time, so what is left says whether
// the limiter still remembers the key.
func spend(t *testing.T, e *graphql.Executor, key string) (available int, throttled bool) {
	t.Helper()
	resp := e.Execute(shopCtx(key), &graphql.Request{Query: `{ items(first: 59) { id } }`})
	defer resp.Release()
	return status(t, resp)["currentlyAvailable"].(int), len(resp.Errors) > 0
}

func boundedExec(t *testing.T, maxBuckets int) *graphql.Executor {
	t.Helper()
	now := time.Unix(0, 0)
	return newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 1}),
		throttle.New(throttle.Config{
			MaximumAvailable: 100, RestoreRate: 0, Key: keyFunc, Now: fixedClock(&now), MaxBuckets: maxBuckets,
		}))
}

// The key is whatever identifies a caller, and a caller can be anyone: with no
// bound on the buckets held, a flood of distinct keys is a flood of memory.
// What the bound costs is in the last assertion, and Config.MaxBuckets says so.
func TestBucketsBeyondMaxBucketsAreEvicted(t *testing.T) {
	e := boundedExec(t, 2)
	for _, key := range []string{"a", "b", "c"} {
		if got, throttled := spend(t, e, key); throttled || got != 40 {
			t.Fatalf("%s: available = %d, throttled = %v, want a first spend leaving 40", key, got, throttled)
		}
	}
	// b and c are the two held: each remembers what it spent.
	for _, key := range []string{"b", "c"} {
		if _, throttled := spend(t, e, key); !throttled {
			t.Fatalf("%s was forgotten while within the bound", key)
		}
	}
	// a was the least recently used of three, and a forgotten caller starts
	// full.
	if got, throttled := spend(t, e, "a"); throttled || got != 40 {
		t.Fatalf("a: available = %d, throttled = %v; it is still held, so three buckets are under a bound of two", got, throttled)
	}
}

// Eviction takes the least recently used bucket, not the oldest. Taking the
// oldest forgets the caller who is busiest, which is the one being limited.
func TestABucketInUseIsNotTheOneEvicted(t *testing.T) {
	e := boundedExec(t, 2)
	spend(t, e, "a")
	spend(t, e, "b")
	if _, throttled := spend(t, e, "a"); !throttled {
		t.Fatal("a: a second spend should have been refused")
	}
	spend(t, e, "c")

	if _, throttled := spend(t, e, "a"); !throttled {
		t.Fatal("a was evicted though it was used after b")
	}
	if got, throttled := spend(t, e, "b"); throttled || got != 40 {
		t.Fatalf("b: available = %d, throttled = %v, want it evicted and full again", got, throttled)
	}
}

// A refund lands after the query ran, and the bucket refilled while it did.
// Added on top of a bucket that is already full, it would leave the caller
// more than the maximum to spend at once.
func TestARefundDoesNotOverfillABucketThatRefilledMeanwhile(t *testing.T) {
	// Every reading is a thousand seconds after the last, so the bucket is
	// full again by the time the refund reads the clock.
	now := time.Unix(0, 0)
	clock := func() time.Time {
		now = now.Add(1000 * time.Second)
		return now
	}
	e := newExec(t,
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10, Actual: true}),
		throttle.New(throttle.Config{MaximumAvailable: 100, RestoreRate: 1, Key: keyFunc, Now: clock}))

	// Quoted 11, really 2: nine points come back.
	resp := e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items { id } }`})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := status(t, resp)["currentlyAvailable"]; got != 100 {
		t.Fatalf("currentlyAvailable = %v, want the maximum of 100", got)
	}
}

// A query the cost limit refuses resolved nothing. Keeping its quote charged a
// caller for work that was never done, and a client that sent an oversized
// query a few times was then throttled on the small ones.
func TestARequestRefusedBeforeItRanIsNotCharged(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		throttle.New(throttle.Config{MaximumAvailable: 100, RestoreRate: 0, Key: keyFunc, Now: fixedClock(&now)}),
		graphql.WithQueryCost(graphql.QueryCost{Max: 10, DefaultListSize: 1}))

	for range 3 {
		resp := e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items(first: 40) { id } }`})
		if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != graphql.CodeTooComplex {
			t.Fatalf("errors = %v, want the cost limit's refusal", resp.Errors)
		}
		if got := status(t, resp)["currentlyAvailable"]; got != 100 {
			t.Fatalf("currentlyAvailable = %v after a query that never ran, want 100", got)
		}
		resp.Release()
	}
}

// Without Actual there is no measurement to refund against, so a query that
// ran keeps its quote, errors or not: the work was done.
func TestARequestThatRanKeepsItsQuote(t *testing.T) {
	now := time.Unix(0, 0)
	e := newExec(t,
		throttle.New(throttle.Config{MaximumAvailable: 100, RestoreRate: 0, Key: keyFunc, Now: fixedClock(&now)}),
		graphql.WithQueryCost(graphql.QueryCost{DefaultListSize: 10}))

	resp := e.Execute(shopCtx("shop-1"), &graphql.Request{Query: `{ items { id } }`})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if got := status(t, resp)["currentlyAvailable"]; got != 89 {
		t.Fatalf("currentlyAvailable = %v, want 89: the quote of 11 stands", got)
	}
}
