package graphql

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// TestWaveCoordinatorNilIsInert pins the documented contract that a nil
// coordinator is usable. Extensions reach one through Waves, which returns
// nil outside an Execute call, and must not have to check.
func TestWaveCoordinatorNilIsInert(t *testing.T) {
	var w *WaveCoordinator
	w.OnReady(func() { t.Fatal("a nil coordinator must not run callbacks") })
	w.Park()
	w.Unpark()

	var oc *OperationContext
	if got := oc.Waves(); got != nil {
		t.Fatalf("(*OperationContext)(nil).Waves() = %v, want nil", got)
	}
	if got := (&OperationContext{}).Waves(); got != nil {
		t.Fatalf("Waves() outside an executor = %v, want nil", got)
	}
}

// TestWaveCoordinatorDispatchesWhenWaveParks walks the protocol the executor
// drives: a wave is announced, its tasks begin, and dispatch happens only
// once every in-flight task is parked -- not before.
func TestWaveCoordinatorDispatchesWhenWaveParks(t *testing.T) {
	w := newWaveCoordinator()
	flushes := 0
	w.OnReady(func() { flushes++ })

	w.push(2)
	w.taskBegin()
	w.taskBegin()

	w.Park()
	if flushes != 0 {
		t.Fatalf("dispatched after 1 of 2 tasks parked; keys would miss the batch")
	}
	w.Park()
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 once every in-flight task is parked", flushes)
	}

	w.Unpark()
	w.Unpark()
	w.taskEnd()
	w.taskEnd()
	w.pop()
}

// TestWaveCoordinatorWaitsForLateTask covers the case the wave bookkeeping
// exists for: a task that has not begun yet must hold back dispatch, or its
// keys are loaded in a second batch.
func TestWaveCoordinatorWaitsForLateTask(t *testing.T) {
	w := newWaveCoordinator()
	flushes := 0
	w.OnReady(func() { flushes++ })

	w.push(2)
	w.taskBegin()
	w.Park()
	if flushes != 0 {
		t.Fatal("dispatched before the second task began")
	}

	w.taskBegin()
	w.Park()
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 after the late task parked", flushes)
	}
}

// TestWaveCoordinatorScheduleFallback covers Park outside any announced wave,
// such as a sequential Load: there is no wave to complete, so dispatch has to
// happen on the next scheduler tick instead.
//
// synctest runs the dispatch goroutine in a bubble, so Wait returns once it
// has finished rather than after a real timeout.
func TestWaveCoordinatorScheduleFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWaveCoordinator()
		var flushes atomic.Int32
		w.OnReady(func() { flushes.Add(1) })

		w.Park()
		synctest.Wait()

		if got := flushes.Load(); got != 1 {
			t.Fatalf("flushes = %d, want 1 after the fallback tick", got)
		}
	})
}

// A wave whose announced tasks have all begun and all ended has nothing in
// flight, so ready can never be true for it again. writeFieldsConcurrent
// leaves exactly that state on the stack while it writes the fields that are
// not schedulable, which is where an Inline() resolver's Load parks. Without
// the fallback the caller waits for its deadline.
func TestWaveCoordinatorFallsBackWhenTheWaveIsDead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWaveCoordinator()
		var flushes atomic.Int32
		w.OnReady(func() { flushes.Add(1) })

		w.push(2)
		for range 2 {
			w.taskBegin()
			w.taskEnd()
		}
		// Inline, after g.wait(), with the wave still pushed.
		w.Park()
		synctest.Wait()

		if got := flushes.Load(); got != 1 {
			t.Fatalf("flushes = %d, want 1: a park into a spent wave was never dispatched", got)
		}
		w.Unpark()
	})
}

// The fallback must not fire while the wave can still become ready, or a
// batch dispatches before its siblings have queued their keys and batching
// degrades towards N+1.
func TestWaveCoordinatorDoesNotFallBackWhileATaskIsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWaveCoordinator()
		var flushes atomic.Int32
		w.OnReady(func() { flushes.Add(1) })

		w.push(2)
		w.taskBegin()
		w.taskBegin()
		// One of the two parks; the other is still working.
		w.Park()
		synctest.Wait()
		if got := flushes.Load(); got != 0 {
			t.Fatalf("flushes = %d, want 0 while a sibling is still in flight", got)
		}
		// When it parks too, the wave is ready and they dispatch together.
		w.Park()
		if got := flushes.Load(); got != 1 {
			t.Fatalf("flushes = %d, want 1 once every in-flight task has parked", got)
		}
	})
}
