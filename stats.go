package graphql

import (
	"log/slog"
	"sync/atomic"
)

// ExecutorStats is a point-in-time view of an Executor's shared state, for
// exporting as metrics (ext/otel.ObserveExecutor does). Taking one is cheap
// and safe from any goroutine.
type ExecutorStats struct {
	// PlanCacheEntries and PlanCacheBytes are the documents the plan cache
	// holds and what they are charged, their query text, their parsed form
	// and what their plans retain: the quantity WithPlanCacheBytes bounds.
	PlanCacheEntries int
	PlanCacheBytes   int64

	// PlanCacheHits and PlanCacheMisses count operations that were planned: a
	// hit found its compiled plan cached, a miss compiled one. A request
	// rejected by parsing, validation or a depth, complexity or plan field
	// limit is neither, and so is OperationKind, which transports call before
	// Execute; one refused by the query cost limit has already been planned
	// and counts. A document with too many conditional variables to cache its
	// plans (OperationStats.PlanUncacheable) is a miss on every request. A
	// subscription counts once, when it opens: its events reuse that plan.
	// Per operation this is the judgement OperationStats.CacheHit reports,
	// though CacheHit is repeated on every subscription event.
	PlanCacheHits   int64
	PlanCacheMisses int64

	// ConcurrencyInUse and ConcurrencyLimit are the resolver slots held right
	// now and the WithMaxConcurrency budget; both are zero when concurrency is
	// disabled.
	ConcurrencyInUse int
	ConcurrencyLimit int

	// RequestsUnbounded is true when nothing bounds what one request may ask
	// for: none of WithMaxDepth, WithMaxComplexity and a WithQueryCost Max is
	// set. It is the default, because the right numbers belong to a schema
	// and not to this package, and it is here so that the default shows on a
	// dashboard rather than in the incident that found it.
	RequestsUnbounded bool
}

// Stats reports the executor's current shared state.
func (e *Executor) Stats() ExecutorStats {
	entries, bytes := e.cache.occupancy()
	return ExecutorStats{
		PlanCacheEntries: entries,
		PlanCacheBytes:   bytes,
		PlanCacheHits:    e.planHits.Load(),
		PlanCacheMisses:  e.planMisses.Load(),
		ConcurrencyInUse: len(e.sem),
		ConcurrencyLimit: cap(e.sem),

		RequestsUnbounded: e.requestsUnbounded(),
	}
}

// requestsUnbounded reports that no option bounds the size of an operation.
// A QueryCost with no Max prices operations and refuses none.
func (e *Executor) requestsUnbounded() bool {
	return e.maxDepth == 0 && e.maxComplexity == 0 && (e.cost == nil || e.cost.Max == 0)
}

// unboundedSaid is set once this process has been told an executor is
// unbounded: the line is for whoever reads the log at start-up, and a process
// that builds an executor per tenant has no use for it per tenant.
var unboundedSaid atomic.Bool

func (e *Executor) sayIfUnbounded() {
	if e.requestsUnbounded() && unboundedSaid.CompareAndSwap(false, true) {
		slog.Warn("graphql: this executor has no depth, complexity or cost limit, so one request can ask for any amount of work; set WithMaxDepth, WithMaxComplexity or WithQueryCost for this schema, or bound what clients may send another way (ExecutorStats.RequestsUnbounded reports this)")
	}
}

func (e *Executor) countPlan(hit bool) {
	if hit {
		e.planHits.Add(1)
	} else {
		e.planMisses.Add(1)
	}
}
