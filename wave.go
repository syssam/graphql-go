package graphql

import "sync"

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
//	w.Unpark()        // once for each Park, before the parked goroutine is woken
//
// When nothing in the operation is left running -- every task begun, and each
// either parked or waiting for tasks of its own -- the registered callbacks
// run so the keys queued by all of them dispatch together. The
// loader package is built on this; most applications use that instead.
//
// Unpark belongs to whoever ends the wait, and for a batch that is the
// callback, not the goroutines it wakes: it calls Unpark once for each of
// them and only then hands out the results. A goroutine left to report itself
// still counts as parked from the moment it is woken until it is scheduled,
// and for that long the operation looks idle; the first of its siblings to
// reach its next Park is then dispatched alone, and what should have been one
// batch becomes one per key. A goroutine that stops waiting for another
// reason, such as its context ending, calls Unpark itself, and the two must
// agree which of them did.
//
// A nil *WaveCoordinator is valid and does nothing, which is what Waves
// returns outside an Execute call.
//
// Experimental: loader is the only thing that has driven this protocol, and
// it may change before v1; see docs/api-stability.md.
type WaveCoordinator struct {
	mu sync.Mutex
	// The counts are for the whole operation, not per wave. A stack of waves
	// was tried first and was wrong: sibling subtrees push and pop their waves
	// concurrently, so a park, a task or a pop landed on whichever wave
	// happened to be on top, and a Load could wait for its deadline
	// (TestLoadIsFlushedWhenWavesPopOutOfOrder).
	//
	// pending is announced tasks not yet begun. running is goroutines doing
	// the operation's work: the one that started it, and every task that has
	// begun, less those parked on batched work or waiting for their own tasks.
	// A park by a goroutine the executor did not start can take it below
	// zero, which reads the same as zero.
	//
	// waiting is goroutines parked. An Unpark made on a goroutine's behalf can
	// arrive before the Park it answers -- its batch finished between its
	// queueing a key and parking -- so waiting dips below zero until that Park
	// lands. running is one too high for exactly as long, which is what keeps
	// the dip from reading as a wave to dispatch.
	pending int32
	running int32
	waiting int32
	flushes []func()
}

// Waves returns the coordinator for this operation. It is nil when the
// operation is not being executed, in which case extensions fall back to
// their own scheduling: outside an Executor, and on the context a
// subscription's interceptors and source are given, which outlives every
// event and is executed for none of them.
func (oc *OperationContext) Waves() *WaveCoordinator {
	if oc == nil {
		return nil
	}
	return oc.hub
}

// startWaves gives oc its coordinator, counting the calling goroutine as the
// one running the operation.
func (oc *OperationContext) startWaves() {
	oc.wave.running = 1
	oc.hub = &oc.wave
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

// Park reports that the caller is about to block on batched work. When that
// leaves nothing running and no announced task still to begin, every key the
// operation can queue before it blocks has been queued, so the ready
// callbacks run.
func (w *WaveCoordinator) Park() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.running--
	w.waiting++
	w.unlockAndFlushIfReady()
}

// Unpark reports that a parked goroutine is running again. It is called once
// for each Park, by the goroutine itself or, before waking it, by the one
// that ends its wait.
func (w *WaveCoordinator) Unpark() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.waiting--
	w.running++
	w.mu.Unlock()
}

// announce reports n sibling tasks about to be launched. It must precede
// their launch: until they begin, they hold back dispatch.
func (w *WaveCoordinator) announce(n int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.pending += int32(n)
	w.mu.Unlock()
}

func (w *WaveCoordinator) taskBegin() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.pending--
	w.running++
	w.mu.Unlock()
}

func (w *WaveCoordinator) taskEnd() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.running--
	w.unlockAndFlushIfReady()
}

// block and resume bracket a goroutine waiting for the tasks it launched,
// which is not running any more than a parked one is.
func (w *WaveCoordinator) block() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.running--
	w.unlockAndFlushIfReady()
}

func (w *WaveCoordinator) resume() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.running++
	w.mu.Unlock()
}

// unlockAndFlushIfReady releases w.mu, running the callbacks outside it when
// the operation is ready to dispatch.
func (w *WaveCoordinator) unlockAndFlushIfReady() {
	if w.waiting <= 0 || w.pending > 0 || w.running > 0 {
		w.mu.Unlock()
		return
	}
	fns := append([]func(){}, w.flushes...)
	w.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
