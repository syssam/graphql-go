// Package loader provides a Facebook-style DataLoader for graphql-go:
// a per-request cache plus a batch function that coalesces the keys
// requested by concurrent resolvers into one call.
//
// Batching is driven by the executor's wave dispatch
// (graphql.WaveCoordinator), so Loads issued by sibling Resolve fields in
// the same wave flush together. Outside an Execute call a Loader still
// works, falling back to its own scheduler; wrap the context with WithScope
// to give such calls a cache of their own.
//
// # When a batch runs
//
// A wave is dispatched when nothing in the operation is left running: every
// resolver has returned, is waiting in a Load, or is waiting for fields of its
// own. Every Loader with keys queued then runs, each on a goroutine of its own
// so two loaders cost the slower of their round trips and not the sum, and the
// results of all of them are handed back together. The resolvers that were
// waiting go on as one wave, so whatever they load next is one batch per
// Loader again, however deep the chain.
//
// That rule has two costs, and both are deliberate: dispatching any earlier
// would mean dispatching while a key could still be queued.
//
// A batch waits for the slowest resolver in the operation, including one that
// has nothing to do with it. Beside a field that takes 300ms to call another
// service, a loader's first query starts at 300ms.
//
// A resolver blocked on anything but a Load is running as far as the executor
// can tell. If what it waits for is held by a resolver that is itself inside
// Load -- a mutex kept across the call, a singleflight group, a semaphore of
// size one -- the wave is never idle, nothing is dispatched, and the request
// hangs until its context ends, which without WithOperationTimeout or a
// deadline of the caller's is never. Do not hold a lock across Load or
// LoadMany.
package loader

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// BatchFunc loads many keys in one call. The returned map may omit keys;
// those Loads receive the zero value and a nil error (not found). A non-nil
// error fails every waiter in that batch and is not cached.
//
// Keys are unique. The function is called from a single goroutine per batch,
// and the chunks WithMaxBatchSize splits a flush into are called one after the
// other; batches of different Loaders in one wave run at the same time. A
// panic is recovered, logged, and fails every waiter in the batch with an
// INTERNAL_SERVER_ERROR that does not carry the panic value.
//
// ctx carries the values of a Load waiting on the batch, so a tracing span or
// tenant read from it is a caller's own. It is cancelled only once every Load
// waiting on the batch has given up, and its deadline is the latest of theirs:
// a batch serves several resolvers, and one of them returning early, or
// running under a shorter timeout of its own, must not fail its siblings.
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
// calls in one resolver do not coalesce; use LoadMany for that. The package
// documentation says when a wave is dispatched and what that costs.
type Loader[K comparable, V any] struct {
	batch    BatchFunc[K, V]
	mapped   MappedBatchFunc[K, V]
	maxBatch int
	cache    bool

	// orphan is used when Load is called outside Execute and outside
	// WithScope. It never caches: it is shared by every such caller for the
	// life of the process, so a cache would grow without bound and hand one
	// caller's result to another.
	orphan requestScope[K, V]
}

type scopeKey struct{}

type scopeSet struct {
	mu sync.Mutex
	m  map[any]any
}

// WithScope returns a context under which every Loader keeps one cache and
// one pending queue, the way it does for one request under Execute. It is for
// work outside Execute -- a background job, a script, a test -- where there
// is no request to scope them to. Without it such calls still batch, but
// cache nothing, because the only thing left to share a cache with is every
// other caller in the process.
func WithScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scopeSet{m: make(map[any]any)})
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
	l.orphan.init(l, nil, false)
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
	l.orphan.init(l, nil, false)
	return l
}

// Load returns the value for key, batching with other Loads in the same
// request wave when possible.
//
// It returns once the wave has been dispatched, which is when no resolver in
// the operation is still running, so it waits for the slowest of them. Never
// call it holding a lock, or anything else a sibling resolver may block on:
// that resolver counts as running, the wave is never dispatched, and both wait
// until the request's context ends.
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
	// An operation with no coordinator is not being executed: it is the
	// context a subscription's interceptors and source run under, which lives
	// as long as the stream does. A cache scoped to it would answer every
	// event from what the first one loaded, so it is treated as no request at
	// all.
	w := oc.Waves()
	if w == nil {
		sc, _ := ctx.Value(scopeKey{}).(*scopeSet)
		if sc == nil {
			return &l.orphan
		}
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if v, ok := sc.m[l]; ok {
			return v.(*requestScope[K, V])
		}
		s := &requestScope[K, V]{}
		s.init(l, nil, l.cache)
		sc.m[l] = s
		return s
	}
	if v, ok := oc.Get(l); ok {
		return v.(*requestScope[K, V])
	}
	d := dispatcherFor(oc, w)
	s := &requestScope[K, V]{}
	s.init(l, w, l.cache)
	// Published and enrolled under the dispatcher's lock. A sibling can find
	// the scope the moment it is published and park on it, and the dispatch
	// that answers takes the same lock first, so it cannot run before the
	// scope is one it knows to ask.
	d.mu.Lock()
	defer d.mu.Unlock()
	// Scopes are only ever added, and under this lock, so none yet is the
	// first Load of the operation.
	if len(d.scopes) == 0 {
		w.OnReady(d.flush)
	}
	// GetOrSet, not Set: sibling resolvers reach their first Load together
	// and would otherwise each install a scope, splitting the pending queue
	// and the cache so batching degrades to N+1.
	if actual, loaded := oc.GetOrSet(l, s); loaded {
		return actual.(*requestScope[K, V])
	}
	d.scopes = append(d.scopes, s)
	return s
}

type dispatcherKey struct{}

// dispatcher runs one operation's waves for every Loader used in it. It is
// one callback on the coordinator rather than one per Loader because a wave is
// one thing: every batch in it is run, and only then is any result delivered.
// Per Loader, each delivered as its own batch function returned, so the
// resolvers of the first went on, queued their next keys and were dispatched
// for before the second Loader's resolvers had been woken.
type dispatcher struct {
	waves *graphql.WaveCoordinator

	mu     sync.Mutex
	scopes []batchSource
}

// batchSource is a requestScope with its key and value types erased.
type batchSource interface {
	take() (pendingBatch, bool)
}

// pendingBatch is the keys one scope had queued when a wave was dispatched.
type pendingBatch interface {
	// run calls the batch function and keeps what it returned.
	run()
	// resume reports every Load waiting on the batch as running again.
	resume(w *graphql.WaveCoordinator)
	// deliver hands each waiter its result.
	deliver()
}

func dispatcherFor(oc *graphql.OperationContext, w *graphql.WaveCoordinator) *dispatcher {
	if v, ok := oc.Get(dispatcherKey{}); ok {
		return v.(*dispatcher)
	}
	actual, _ := oc.GetOrSet(dispatcherKey{}, &dispatcher{waves: w})
	return actual.(*dispatcher)
}

// flush dispatches one wave. It takes what is queued once and does not look
// again: a key queued after that belongs to the wave its Load parks into, and
// taking it here would dispatch it ahead of its siblings.
func (d *dispatcher) flush() {
	d.mu.Lock()
	scopes := d.scopes
	d.mu.Unlock()

	var buf [4]pendingBatch
	batches := buf[:0]
	for _, s := range scopes {
		if b, ok := s.take(); ok {
			batches = append(batches, b)
		}
	}
	switch len(batches) {
	case 0:
		return
	case 1:
		batches[0].run()
	default:
		var wg sync.WaitGroup
		for _, b := range batches[1:] {
			wg.Go(b.run)
		}
		batches[0].run()
		wg.Wait()
	}
	// Every waiter is reported running before any is woken. A woken waiter
	// that still counted as parked left the operation looking idle until it
	// was scheduled, and the first of its siblings to reach its next Load
	// dispatched that Load alone.
	for _, b := range batches {
		b.resume(d.waves)
	}
	for _, b := range batches {
		b.deliver()
	}
}

type result[V any] struct {
	val V
	err error
}

type waiter[V any] struct {
	ctx context.Context
	ch  chan result[V]
	// park is the state of the Load this waiter belongs to, shared by every
	// waiter of one LoadMany. It is nil where there is no coordinator.
	park *atomic.Int32
}

// A Load that waits on a coordinator is queued, then parked, then resumed.
// Park and Unpark must pair exactly, and two goroutines can each be the one
// to report the Load running again: the dispatch that is about to wake it, and
// the Load itself when its context ends first. Whoever moves the state to
// resumed is the one that calls Unpark. A dispatch can also take a Load's keys
// before the Load has parked, when the count was brought to idle by goroutines
// the executor does not know about; that Load is resumed from queued and never
// parks at all.
const (
	loadQueued int32 = iota
	loadParked
	loadResumed
)

type requestScope[K comparable, V any] struct {
	loader    *Loader[K, V]
	waves     *graphql.WaveCoordinator
	mu        sync.Mutex
	cache     map[K]V
	cached    map[K]struct{}
	pending   []K
	waiters   map[K][]*waiter[V]
	scheduled atomic.Bool
}

func (s *requestScope[K, V]) init(l *Loader[K, V], w *graphql.WaveCoordinator, cache bool) {
	s.loader = l
	s.waves = w
	if cache {
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
	var park *atomic.Int32

	s.mu.Lock()
	for i, key := range keys {
		if s.cached != nil {
			if _, ok := s.cached[key]; ok {
				out[i] = s.cache[key]
				continue
			}
		}
		if park == nil && s.waves != nil {
			park = new(atomic.Int32)
		}
		w := &waiter[V]{ctx: ctx, ch: make(chan result[V], 1), park: park}
		if _, seen := s.waiters[key]; !seen {
			s.pending = append(s.pending, key)
		}
		s.waiters[key] = append(s.waiters[key], w)
		wait = append(wait, w)
		waitAt = append(waitAt, i)
	}
	s.mu.Unlock()

	if len(wait) == 0 {
		return out, nil
	}
	switch {
	case s.waves == nil:
		s.schedule()
	case park.CompareAndSwap(loadQueued, loadParked):
		s.waves.Park()
		defer func() {
			if park.CompareAndSwap(loadParked, loadResumed) {
				s.waves.Unpark()
			}
		}()
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

func (s *requestScope[K, V]) schedule() {
	if !s.scheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		runtime.Gosched()
		s.flush()
		s.scheduled.Store(false)
		s.mu.Lock()
		more := len(s.pending) > 0
		s.mu.Unlock()
		if more {
			s.schedule()
		}
	}()
}

// flush is the scheduler's dispatch, for a scope with no coordinator.
func (s *requestScope[K, V]) flush() {
	if b, ok := s.take(); ok {
		b.run()
		b.deliver()
	}
}

// take removes everything queued, or reports that nothing is.
func (s *requestScope[K, V]) take() (pendingBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil, false
	}
	// Every queued key has waiters and every key with waiters is queued, so
	// both are handed over whole.
	b := &batch[K, V]{scope: s, keys: s.pending, waiters: s.waiters}
	s.pending = nil
	s.waiters = make(map[K][]*waiter[V])
	return b, true
}

// batch is the keys taken from one scope and, once run, a result for each.
type batch[K comparable, V any] struct {
	scope   *requestScope[K, V]
	keys    []K
	waiters map[K][]*waiter[V]
	results []result[V]
}

func (b *batch[K, V]) run() {
	s := b.scope
	b.results = make([]result[V], len(b.keys))
	for start := 0; start < len(b.keys); {
		end := len(b.keys)
		if lim := s.loader.maxBatch; lim > 0 && end-start > lim {
			end = start + lim
		}
		// Capped, so a batch function that appends to its keys cannot write
		// over the next chunk's.
		keys := b.keys[start:end:end]
		ctx, release := batchContext(keys, b.waiters)
		got, keyErrs, err := s.invoke(ctx, keys)
		release()
		for i, k := range keys {
			r := &b.results[start+i]
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
		}
		start = end
	}
}

func (b *batch[K, V]) resume(w *graphql.WaveCoordinator) {
	for _, ws := range b.waiters {
		for _, wt := range ws {
			for {
				if wt.park.CompareAndSwap(loadParked, loadResumed) {
					w.Unpark()
					break
				}
				// Not parked: either it has yet to, and now will not, or it
				// was already resumed -- by its own context ending, or as
				// another waiter of the same LoadMany.
				if wt.park.CompareAndSwap(loadQueued, loadResumed) || wt.park.Load() == loadResumed {
					break
				}
			}
		}
	}
}

func (b *batch[K, V]) deliver() {
	for i, k := range b.keys {
		for _, w := range b.waiters[k] {
			w.ch <- b.results[i]
		}
	}
}

// batchContext derives the context a batch runs under from the Loads waiting
// on it. Taking any one caller's context whole was wrong: a resolver that
// wraps its context in a timeout and returns cancels it, and every later batch
// in the request then failed with context.Canceled.
func batchContext[K comparable, V any](keys []K, waiters map[K][]*waiter[V]) (context.Context, func()) {
	// Distinct by Done channel, not by identity: under Execute every
	// resolver's context is its own value-only wrapper around the request's,
	// so identities never repeat, while their cancellation is one channel.
	// Two contexts sharing a Done channel are cancelled together, so either
	// stands for both. Waiters overwhelmingly share one, so a linear scan
	// over a stack buffer beats a map.
	var buf [4]context.Context
	var dones [4]<-chan struct{}
	ctxs, doneOf := buf[:0], dones[:0]
	for _, k := range keys {
		for _, w := range waiters[k] {
			if w.ctx == nil {
				continue
			}
			if d := w.ctx.Done(); !slices.Contains(doneOf, d) {
				ctxs, doneOf = append(ctxs, w.ctx), append(doneOf, d)
			}
		}
	}
	if len(ctxs) == 0 {
		return context.Background(), func() {}
	}
	if len(ctxs) == 1 {
		return ctxs[0], func() {}
	}
	base := context.WithoutCancel(ctxs[0])
	var cancelDeadline context.CancelFunc = func() {}
	if dl, ok := latestDeadline(ctxs); ok {
		base, cancelDeadline = context.WithDeadline(base, dl)
	}
	ctx, cancel := context.WithCancel(base)
	var live atomic.Int32
	live.Store(int32(len(ctxs)))
	stops := make([]func() bool, len(ctxs))
	for i, c := range ctxs {
		stops[i] = context.AfterFunc(c, func() {
			if live.Add(-1) == 0 {
				cancel()
			}
		})
	}
	return ctx, func() {
		for _, stop := range stops {
			stop()
		}
		cancel()
		cancelDeadline()
	}
}

// latestDeadline reports the latest deadline among ctxs, and false when any
// of them has none: a waiter without a deadline would wait indefinitely, so
// the batch must not impose one on its behalf.
func latestDeadline(ctxs []context.Context) (time.Time, bool) {
	var latest time.Time
	for _, c := range ctxs {
		dl, ok := c.Deadline()
		if !ok {
			return time.Time{}, false
		}
		if dl.After(latest) {
			latest = dl
		}
	}
	return latest, true
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
