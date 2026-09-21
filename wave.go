package graphql

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// WaveCoordinator lets request-scoped batching extensions take part in the
// executor's dispatch of concurrent waves. A wave is announced before
// sibling tasks are launched, so a resolver that blocks on batched work
// cannot dispatch before its siblings have queued theirs — the GraphQL
// equivalent of DataLoader.dispatch at the end of a tick.
//
// The protocol for an extension is:
//
//	w := graphql.OperationFrom(ctx).Waves()
//	w.OnReady(flush)  // once per operation
//	w.Park()          // before blocking on batched work
//	defer w.Unpark()
//
// When every in-flight task in the current wave is parked, the registered
// callbacks run so the keys queued by all of them dispatch together. The
// loader package is built on this; most applications use that instead.
//
// A nil *WaveCoordinator is valid and does nothing, which is what Waves
// returns outside an Execute call.
type WaveCoordinator struct {
	mu        sync.Mutex
	stack     []waveState
	flushes   []func()
	scheduled atomic.Bool
}

// waveState tracks one level of announced sibling tasks. A wave is ready to
// dispatch once every announced task has begun and every task still in
// flight is parked.
type waveState struct {
	announced int
	begun     int
	ended     int
	waiting   int
}

func newWaveCoordinator() *WaveCoordinator { return &WaveCoordinator{} }

// Waves returns the coordinator for this operation. It is nil when the
// operation is not running under an Executor, in which case extensions fall
// back to their own scheduling.
func (oc *OperationContext) Waves() *WaveCoordinator {
	if oc == nil {
		return nil
	}
	return oc.hub
}

// OnReady registers a callback to run when a wave is ready to dispatch.
// Callbacks are kept for the life of the operation and must be safe to call
// when there is nothing queued.
func (w *WaveCoordinator) OnReady(fn func()) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.flushes = append(w.flushes, fn)
	w.mu.Unlock()
}

// Park reports that the caller is about to block on batched work. It runs
// the ready callbacks when parking completes the current wave, and falls back
// to the next scheduler tick both outside any wave and inside one that is
// spent -- every announced task begun and ended -- since such a wave can
// never complete again and the caller would otherwise wait for its deadline.
func (w *WaveCoordinator) Park() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if n := len(w.stack); n > 0 {
		st := &w.stack[n-1]
		st.waiting++
		if w.ready(st) {
			fns := append([]func(){}, w.flushes...)
			w.mu.Unlock()
			w.runFlushes(fns)
			return
		}
		// ready needs a task in flight. A wave whose announced tasks have all
		// begun and all ended has none and will never have another, so a
		// caller parking into it waits for its deadline: that is where
		// writeFieldsConcurrent writes a field that is not schedulable, after
		// g.wait() and before the deferred pop, which is exactly where an
		// Inline() resolver's Load runs. Fall back to the tick there, the same
		// as parking outside any wave.
		dead := st.begun >= st.announced && st.begun == st.ended
		w.mu.Unlock()
		if dead {
			w.schedule()
		}
		return
	}
	w.mu.Unlock()
	w.schedule()
}

// Unpark reports that the caller is no longer blocked.
func (w *WaveCoordinator) Unpark() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if n := len(w.stack); n > 0 && w.stack[n-1].waiting > 0 {
		w.stack[n-1].waiting--
	}
	w.mu.Unlock()
}

// push announces n sibling tasks about to be launched.
func (w *WaveCoordinator) push(n int) {
	w.mu.Lock()
	w.stack = append(w.stack, waveState{announced: n})
	w.mu.Unlock()
}

func (w *WaveCoordinator) pop() {
	w.mu.Lock()
	if n := len(w.stack); n > 0 {
		w.stack = w.stack[:n-1]
	}
	w.mu.Unlock()
}

func (w *WaveCoordinator) taskBegin() {
	w.mu.Lock()
	if n := len(w.stack); n > 0 {
		w.stack[n-1].begun++
	}
	w.mu.Unlock()
}

func (w *WaveCoordinator) taskEnd() {
	w.mu.Lock()
	var fns []func()
	if n := len(w.stack); n > 0 {
		w.stack[n-1].ended++
		if w.ready(&w.stack[n-1]) {
			fns = append(fns, w.flushes...)
		}
	}
	w.mu.Unlock()
	w.runFlushes(fns)
}

func (w *WaveCoordinator) ready(s *waveState) bool {
	if s.waiting == 0 || s.begun < s.announced {
		return false
	}
	inFlight := s.begun - s.ended
	return inFlight > 0 && s.waiting >= inFlight
}

// schedule dispatches on the next scheduler tick. It is the fallback for
// Park outside any announced wave, such as a sequential Load.
func (w *WaveCoordinator) schedule() {
	if !w.scheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		runtime.Gosched()
		w.scheduled.Store(false)
		w.mu.Lock()
		fns := append([]func(){}, w.flushes...)
		w.mu.Unlock()
		w.runFlushes(fns)
	}()
}

func (w *WaveCoordinator) runFlushes(fns []func()) {
	for _, fn := range fns {
		fn()
	}
}
