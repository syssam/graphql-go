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
//	defer w.Unpark()
//
// When nothing in the operation is left running -- every task begun, and each
// either parked or waiting for tasks of its own -- the registered callbacks
// run so the keys queued by all of them dispatch together. The
// loader package is built on this; most applications use that instead.
//
// A nil *WaveCoordinator is valid and does nothing, which is what Waves
// returns outside an Execute call.
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
	pending int32
	running int32
	waiting int32
	flushes []func()
}

// Waves returns the coordinator for this operation. It is nil when the
// operation is not running under an Executor, in which case extensions fall
// back to their own scheduling.
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

// Unpark reports that the caller is no longer blocked.
func (w *WaveCoordinator) Unpark() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.waiting > 0 {
		w.waiting--
	}
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
	if w.waiting == 0 || w.pending > 0 || w.running > 0 {
		w.mu.Unlock()
		return
	}
	fns := append([]func(){}, w.flushes...)
	w.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
