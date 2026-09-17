package graphql

// ExecutorStats is a point-in-time view of an Executor's shared state, for
// exporting as metrics (ext/otel.ObserveExecutor does). Taking one is cheap
// and safe from any goroutine.
type ExecutorStats struct {
	// PlanCacheEntries and PlanCacheBytes are the documents the plan cache
	// holds and their total query text, the quantity WithPlanCacheBytes bounds.
	PlanCacheEntries int
	PlanCacheBytes   int64

	// PlanCacheHits and PlanCacheMisses count operations that reached a plan:
	// a hit found its compiled plan cached, a miss compiled one. A request
	// rejected before planning — by parsing, validation or a limit — is
	// neither, and so is OperationKind, which transports call before Execute.
	// They are the same judgement the per-operation CacheHit reports.
	PlanCacheHits   int64
	PlanCacheMisses int64

	// ConcurrencyInUse and ConcurrencyLimit are the resolver slots held right
	// now and the WithMaxConcurrency bound; both are zero when concurrency is
	// disabled.
	ConcurrencyInUse int
	ConcurrencyLimit int
}

// Stats reports the executor's current shared state.
func (e *Executor) Stats() ExecutorStats {
	return ExecutorStats{
		PlanCacheEntries: e.cache.len(),
		PlanCacheBytes:   e.cache.bytes(),
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
