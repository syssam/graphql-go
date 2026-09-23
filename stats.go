package graphql

// ExecutorStats is a point-in-time view of an Executor's shared state, for
// exporting as metrics (ext/otel.ObserveExecutor does). Taking one is cheap
// and safe from any goroutine.
type ExecutorStats struct {
	// PlanCacheEntries and PlanCacheBytes are the documents the plan cache
	// holds and their total query text, the quantity WithPlanCacheBytes bounds.
	PlanCacheEntries int
	PlanCacheBytes   int64

	// PlanCacheHits and PlanCacheMisses count operations that were planned: a
	// hit found its compiled plan cached, a miss compiled one. A request
	// rejected by parsing, validation or a depth or complexity limit is
	// neither, and so is OperationKind, which transports call before Execute;
	// one refused by the query cost limit has already been planned and counts.
	// A document with too many conditional variables to cache its plans
	// (OperationStats.PlanUncacheable) is a miss on every request. A
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
	}
}

func (e *Executor) countPlan(hit bool) {
	if hit {
		e.planHits.Add(1)
	} else {
		e.planMisses.Add(1)
	}
}
