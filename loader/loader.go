// Package loader provides a Facebook-style DataLoader for graphql-go:
// a per-request cache plus a batch function that coalesces the keys
// requested by concurrent resolvers into one call.
//
// Batching is driven by the executor's wave dispatch
// (graphql.WaveCoordinator), so Loads issued by sibling Resolve fields in
// the same wave flush together. Outside an Execute call a Loader still
// works, falling back to its own scheduler.
package loader

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"

	graphql "github.com/syssam/graphql-go"
)

// BatchFunc loads many keys in one call. The returned map may omit keys;
// those Loads receive the zero value and a nil error (not found). A non-nil
// error fails every waiter in that batch and is not cached.
//
// Keys are unique. The function is called from a single goroutine per batch.
// A panic is recovered, logged, and fails every waiter in the batch with an
// INTERNAL_SERVER_ERROR that does not carry the panic value.
//
// ctx is the request's, not a fresh background one: it carries the request's
// cancellation, deadline and values, so a batch stops when the client goes
// away and a tracing span or tenant read from it is the caller's own.
type BatchFunc[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, error)

// MappedBatchFunc is BatchFunc with per-key failure. A key present in the
// error map fails only the Loads waiting on that key; its siblings in the
// same batch are unaffected, which is what GraphQL's per-field error
// contract asks for. The third return fails every key in the batch and is
// for the whole call going wrong, such as the database being unreachable.
//
// Neither kind of error is cached.
type MappedBatchFunc[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, map[K]error, error)

// Loader is a Facebook-style DataLoader: per-request cache plus a batch
// function. Create one Loader per process (it is safe for concurrent
// Execute calls); cache and in-flight batches are scoped to the
// OperationContext of each request.
//
// Concurrent Resolve fields that call Load in the same execution wave are
// flushed together when every in-flight resolver is parked — the GraphQL
// equivalent of DataLoader.dispatch at the end of a tick. Sequential Load
// calls in one resolver do not coalesce; use LoadMany for that.
type Loader[K comparable, V any] struct {
	batch    BatchFunc[K, V]
	mapped   MappedBatchFunc[K, V]
	maxBatch int
	cache    bool

	// orphan is used when Load is called outside Execute (tests, scripts).
	orphan requestScope[K, V]
}

// Option configures a Loader.
type Option func(*config)

type config struct {
	maxBatch int
	cache    bool
}

// WithoutCache disables the per-request memoization of successful loads.
func WithoutCache() Option {
	return func(c *config) { c.cache = false }
}

// WithMaxBatchSize splits a flush into chunks of at most n keys. Zero
// means no split.
func WithMaxBatchSize(n int) Option {
	return func(c *config) { c.maxBatch = n }
}

// New builds a process-wide loader factory around batch.
func New[K comparable, V any](batch BatchFunc[K, V], opts ...Option) *Loader[K, V] {
	cfg := config{cache: true}
	for _, o := range opts {
		o(&cfg)
	}
	l := &Loader[K, V]{batch: batch, maxBatch: cfg.maxBatch, cache: cfg.cache}
	l.orphan.init(l, nil)
	return l
}

// NewMapped builds a loader whose batch function reports failure per key.
// Everything else — caching, wave-driven batching, max batch size — behaves
// as it does for New.
func NewMapped[K comparable, V any](batch MappedBatchFunc[K, V], opts ...Option) *Loader[K, V] {
	cfg := config{cache: true}
	for _, o := range opts {
		o(&cfg)
	}
	l := &Loader[K, V]{mapped: batch, maxBatch: cfg.maxBatch, cache: cfg.cache}
	l.orphan.init(l, nil)
	return l
}

// Load returns the value for key, batching with other Loads in the same
// request wave when possible.
func (l *Loader[K, V]) Load(ctx context.Context, key K) (V, error) {
	var zero V
	out, err := l.scope(ctx).load(ctx, []K{key})
	if err != nil {
		return zero, err
	}
	return out[0], nil
}

// LoadMany loads keys in one batch. Order of the result matches keys.
func (l *Loader[K, V]) LoadMany(ctx context.Context, keys []K) ([]V, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return l.scope(ctx).load(ctx, keys)
}

// Prime seeds the per-request cache so a later Load of key does not hit
// the batch function.
func (l *Loader[K, V]) Prime(ctx context.Context, key K, value V) {
	l.scope(ctx).prime(key, value)
}

// Clear drops key from the per-request cache.
func (l *Loader[K, V]) Clear(ctx context.Context, key K) {
	l.scope(ctx).clear(key)
}

func (l *Loader[K, V]) scope(ctx context.Context) *requestScope[K, V] {
	oc := graphql.OperationFrom(ctx)
	if oc == nil {
		return &l.orphan
	}
	if v, ok := oc.Get(l); ok {
		return v.(*requestScope[K, V])
	}
	s := &requestScope[K, V]{}
	w := oc.Waves()
	s.init(l, w)
	// The batch runs after the resolver that queued its keys has parked, so
	// the scope carries the request context rather than taking one from
	// whichever Load happens to trigger the flush. Cancellation, deadline and
	// request-scoped values reach the batch function only through this.
	s.ctx = ctx
	// GetOrSet, not Set: sibling resolvers reach their first Load together
	// and would otherwise each install a scope, splitting the pending queue
	// and the cache so batching degrades to N+1. Only the winner registers
	// a flush callback.
	if actual, loaded := oc.GetOrSet(l, s); loaded {
		return actual.(*requestScope[K, V])
	}
	w.OnReady(func() { s.flush(s.ctx) })
	return s
}

type result[V any] struct {
	val V
	err error
}

type waiter[V any] struct {
	ch chan result[V]
}

type requestScope[K comparable, V any] struct {
	loader    *Loader[K, V]
	waves     *graphql.WaveCoordinator
	ctx       context.Context
	mu        sync.Mutex
	cache     map[K]V
	cached    map[K]struct{}
	pending   []K
	waiters   map[K][]*waiter[V]
	scheduled atomic.Bool
}

func (s *requestScope[K, V]) init(l *Loader[K, V], w *graphql.WaveCoordinator) {
	s.loader = l
	s.waves = w
	if l.cache {
		s.cache = make(map[K]V)
		s.cached = make(map[K]struct{})
	}
	s.waiters = make(map[K][]*waiter[V])
}

func (s *requestScope[K, V]) prime(key K, value V) {
	if s.cache == nil {
		return
	}
	s.mu.Lock()
	s.cache[key] = value
	s.cached[key] = struct{}{}
	s.mu.Unlock()
}

func (s *requestScope[K, V]) clear(key K) {
	if s.cache == nil {
		return
	}
	s.mu.Lock()
	delete(s.cache, key)
	delete(s.cached, key)
	s.mu.Unlock()
}

func (s *requestScope[K, V]) load(ctx context.Context, keys []K) ([]V, error) {
	out := make([]V, len(keys))
	var wait []*waiter[V]
	var waitAt []int

	s.mu.Lock()
	queued := false
	for i, key := range keys {
		if s.cached != nil {
			if _, ok := s.cached[key]; ok {
				out[i] = s.cache[key]
				continue
			}
		}
		w := &waiter[V]{ch: make(chan result[V], 1)}
		if _, seen := s.waiters[key]; !seen {
			s.pending = append(s.pending, key)
			queued = true
		}
		s.waiters[key] = append(s.waiters[key], w)
		wait = append(wait, w)
		waitAt = append(waitAt, i)
	}
	s.mu.Unlock()

	if len(wait) == 0 {
		return out, nil
	}
	if queued {
		s.notify(ctx)
	} else if s.waves == nil {
		s.schedule(ctx)
	}

	if s.waves != nil {
		s.waves.Park()
		defer s.waves.Unpark()
	}

	for j, w := range wait {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-w.ch:
			if r.err != nil {
				return nil, r.err
			}
			out[waitAt[j]] = r.val
		}
	}
	return out, nil
}

func (s *requestScope[K, V]) notify(ctx context.Context) {
	if s.waves != nil {
		return
	}
	s.schedule(ctx)
}

func (s *requestScope[K, V]) schedule(ctx context.Context) {
	if !s.scheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		runtime.Gosched()
		s.flush(ctx)
		s.scheduled.Store(false)
		s.mu.Lock()
		more := len(s.pending) > 0
		s.mu.Unlock()
		if more {
			s.schedule(ctx)
		}
	}()
}

func (s *requestScope[K, V]) flush(ctx context.Context) {
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.mu.Unlock()
			return
		}
		n := len(s.pending)
		if lim := s.loader.maxBatch; lim > 0 && n > lim {
			n = lim
		}
		keys := append([]K(nil), s.pending[:n]...)
		s.pending = s.pending[n:]
		waiters := make(map[K][]*waiter[V], len(keys))
		for _, k := range keys {
			waiters[k] = s.waiters[k]
			delete(s.waiters, k)
		}
		s.mu.Unlock()

		got, keyErrs, err := s.invoke(ctx, keys)
		for _, k := range keys {
			var r result[V]
			switch {
			case err != nil:
				r.err = err
			case keyErrs[k] != nil:
				r.err = keyErrs[k]
			default:
				if v, ok := got[k]; ok {
					r.val = v
					if s.cached != nil {
						s.mu.Lock()
						s.cache[k] = v
						s.cached[k] = struct{}{}
						s.mu.Unlock()
					}
				}
			}
			for _, w := range waiters[k] {
				w.ch <- r
			}
		}
	}
}

// invoke recovers because nothing above it can. The keys it was called for
// have already left s.waiters, so a panic that escaped would strand every
// sibling Load until its deadline, and on the scheduler's own goroutine it
// would end the process.
func (s *requestScope[K, V]) invoke(ctx context.Context, keys []K) (got map[K]V, keyErrs map[K]error, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "loader: batch function panic",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			got, keyErrs = nil, nil
			err = graphql.Errorf("internal system error").WithCode(graphql.CodeInternal)
		}
	}()
	switch {
	case s.loader.mapped != nil:
		return s.loader.mapped(ctx, keys)
	case s.loader.batch != nil:
		v, err := s.loader.batch(ctx, keys)
		return v, nil, err
	default:
		return nil, nil, errors.New("loader: no batch function")
	}
}
