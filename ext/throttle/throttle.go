// Package throttle rate limits by query cost, the way Shopify's Admin API
// does: every caller holds a bucket of points that refills at a fixed rate,
// a query is quoted before it runs, and what it really cost is charged
// afterwards.
//
// A subscription is charged per event, not once at open: each event runs the
// whole operation chain, and a stream billed once would be an unmetered
// firehose. Size the bucket for that, or key subscriptions separately.
//
// Register it before any other operation interceptor. Interceptors wrap in
// registration order, so one registered earlier runs even for a request the
// limiter rejects.
//
// It needs a cost model, so build the executor with graphql.WithQueryCost.
// Add Actual to that model to charge what the query really resolved rather
// than what it was quoted; without it the quote stands.
package throttle

import (
	"container/list"
	"context"
	"math"
	"sync"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// CodeThrottled is the error code on a rejected request. It is Shopify's
// spelling, which is what a client written against that API already handles.
const CodeThrottled = "THROTTLED"

// Config describes one bucket policy. Every caller identified by Key gets
// its own bucket under it.
type Config struct {
	// MaximumAvailable is the bucket size in cost points, and so also the
	// largest single query that can ever run.
	MaximumAvailable int
	// RestoreRate is points added per second, capped at MaximumAvailable.
	RestoreRate float64
	// Key identifies the caller whose bucket to charge — a shop, an API
	// token, a tenant. Requests whose key is empty share one bucket.
	Key func(ctx context.Context) string
	// MaxBuckets bounds how many callers are tracked, least recently used
	// evicted first. Zero means 10,000. An evicted caller starts full, so
	// this trades memory for strictness under a flood of distinct keys.
	MaxBuckets int
	// Now defaults to time.Now and exists so tests need not sleep.
	Now func() time.Time
}

const defaultMaxBuckets = 10000

// New returns the executor option that installs the limiter.
func New(cfg Config) graphql.ExecutorOption {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxBuckets <= 0 {
		cfg.MaxBuckets = defaultMaxBuckets
	}
	t := &limiter{cfg: cfg, index: make(map[string]*list.Element), order: list.New()}
	return graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(t.intercept))
}

type bucket struct {
	key       string
	available float64
	last      time.Time
}

type limiter struct {
	cfg   Config
	mu    sync.Mutex
	index map[string]*list.Element
	order *list.List
}

func (t *limiter) intercept(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
	key := ""
	if t.cfg.Key != nil {
		key = t.cfg.Key(ctx)
	}
	quoted := oc.Cost()

	if quoted > t.cfg.MaximumAvailable {
		// Refilling will never help, so say so rather than inviting a retry
		// loop that can only fail.
		err := graphql.Errorf("query costs %d, more than the maximum of %d available to any single request; it can never be run",
			quoted, t.cfg.MaximumAvailable).WithCode(CodeThrottled)
		resp := &graphql.Response{Errors: []*graphql.Error{err}}
		t.report(resp, t.peek(key))
		return resp
	}

	available, ok := t.charge(key, quoted)
	if !ok {
		err := graphql.Errorf("throttled: query costs %d and only %d points are available",
			quoted, int(math.Floor(available))).WithCode(CodeThrottled)
		resp := &graphql.Response{Errors: []*graphql.Error{err}}
		t.report(resp, available)
		return resp
	}

	resp := next(ctx, oc)

	// Shopify charges what the query really cost and returns the rest. The
	// quote has to guess how long every list is; the actual figure counted
	// them, so refunding the difference is the honest charge.
	if actual, measured := oc.ActualCost(); measured && actual < quoted {
		available = t.refund(key, float64(quoted-actual))
	}
	t.report(resp, available)
	return resp
}

// charge deducts n from the key's bucket, reporting what is left and whether
// it fit. A rejected request is not charged.
func (t *limiter) charge(key string, n int) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.bucketLocked(key)
	if b.available < float64(n) {
		return b.available, false
	}
	b.available -= float64(n)
	return b.available, true
}

func (t *limiter) refund(key string, n float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.bucketLocked(key)
	b.available = math.Min(b.available+n, float64(t.cfg.MaximumAvailable))
	return b.available
}

// peek restores and reports without charging.
func (t *limiter) peek(key string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bucketLocked(key).available
}

// bucketLocked returns the key's bucket with its refill applied to now.
// Restoring on read rather than on a timer means an idle caller costs
// nothing to track.
func (t *limiter) bucketLocked(key string) *bucket {
	now := t.cfg.Now()
	if el, ok := t.index[key]; ok {
		t.order.MoveToFront(el)
		b := el.Value.(*bucket)
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.available = math.Min(b.available+elapsed*t.cfg.RestoreRate, float64(t.cfg.MaximumAvailable))
		}
		b.last = now
		return b
	}
	b := &bucket{key: key, available: float64(t.cfg.MaximumAvailable), last: now}
	t.index[key] = t.order.PushFront(b)
	for t.order.Len() > t.cfg.MaxBuckets {
		oldest := t.order.Back()
		t.order.Remove(oldest)
		delete(t.index, oldest.Value.(*bucket).key)
	}
	return b
}

// report writes throttleStatus beside the cost the engine reports, which is
// where a Shopify client already looks for it.
func (t *limiter) report(resp *graphql.Response, available float64) {
	if resp == nil {
		return
	}
	if resp.Extensions == nil {
		resp.Extensions = map[string]any{}
	}
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if cost == nil {
		cost = map[string]any{}
		resp.Extensions["cost"] = cost
	}
	cost["throttleStatus"] = map[string]any{
		"maximumAvailable":   t.cfg.MaximumAvailable,
		"currentlyAvailable": int(math.Floor(available)),
		"restoreRate":        t.cfg.RestoreRate,
	}
}
