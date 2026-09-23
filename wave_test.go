package graphql

import (
	"testing"
)

// TestWaveCoordinatorNilIsInert pins the documented contract that a nil
// coordinator is usable. Extensions reach one through Waves, which returns
// nil outside an Execute call, and must not have to check.
func TestWaveCoordinatorNilIsInert(t *testing.T) {
	var w *WaveCoordinator
	w.OnReady(func() { t.Fatal("a nil coordinator must not run callbacks") })
	w.Park()
	w.Unpark()
	w.announce(2)
	w.taskBegin()
	w.taskEnd()
	w.block()
	w.resume()

	var oc *OperationContext
	if got := oc.Waves(); got != nil {
		t.Fatalf("(*OperationContext)(nil).Waves() = %v, want nil", got)
	}
	if got := (&OperationContext{}).Waves(); got != nil {
		t.Fatalf("Waves() outside an executor = %v, want nil", got)
	}
}

// newWaves is a coordinator as an executor starts one: the calling goroutine
// is running the operation.
func newWaves(flushes *int) *WaveCoordinator {
	oc := &OperationContext{}
	oc.startWaves()
	oc.hub.OnReady(func() { *flushes++ })
	return oc.hub
}

// TestWaveCoordinatorDispatchesWhenWaveParks walks the protocol the executor
// drives: tasks are announced, begin, and dispatch happens only once every
// one of them is parked -- not before.
func TestWaveCoordinatorDispatchesWhenWaveParks(t *testing.T) {
	flushes := 0
	w := newWaves(&flushes)

	w.announce(2)
	w.taskBegin()
	w.taskBegin()
	w.block()

	w.Park()
	if flushes != 0 {
		t.Fatalf("dispatched after 1 of 2 tasks parked; keys would miss the batch")
	}
	w.Park()
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 once every task is parked", flushes)
	}
	w.Unpark()
	w.Unpark()
	w.taskEnd()
	w.taskEnd()
	w.resume()
}

// A task that has not begun yet must hold back dispatch, or its keys are
// loaded in a second batch.
func TestWaveCoordinatorWaitsForLateTask(t *testing.T) {
	flushes := 0
	w := newWaves(&flushes)

	w.announce(2)
	w.taskBegin()
	w.block()
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

// The goroutine that launched the tasks holds back dispatch until it waits
// for them: until then it can still start more work that queues keys.
func TestWaveCoordinatorWaitsForTheLauncher(t *testing.T) {
	flushes := 0
	w := newWaves(&flushes)

	w.announce(1)
	w.taskBegin()
	w.Park()
	if flushes != 0 {
		t.Fatal("dispatched while the launching goroutine was still running")
	}
	w.block()
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 once the launcher waits", flushes)
	}
}

// A Load on the goroutine running the operation, with nothing else in flight
// -- a serial mutation field, or an Inline() resolver written after its
// siblings -- has nothing to wait for and dispatches at once.
func TestWaveCoordinatorDispatchesALoneLoad(t *testing.T) {
	flushes := 0
	w := newWaves(&flushes)

	w.Park()
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 for a Load with nothing else running", flushes)
	}
	w.Unpark()

	// And after a group of tasks has finished, from the launcher.
	w.announce(2)
	w.taskBegin()
	w.taskBegin()
	w.block()
	w.taskEnd()
	w.taskEnd()
	w.resume()
	w.Park()
	if flushes != 2 {
		t.Fatalf("flushes = %d, want 2: a Load after the group finished was not dispatched", flushes)
	}
}

// Dispatch must not happen while any task can still queue a key, wherever in
// the operation that task is. A per-wave count saw only its own wave, so a
// sibling subtree still running did not hold it back.
func TestWaveCoordinatorWaitsForEveryRunningSubtree(t *testing.T) {
	flushes := 0
	w := newWaves(&flushes)

	w.announce(2) // a and c
	w.taskBegin()
	w.taskBegin()
	w.block()

	w.announce(2) // a's children
	w.taskBegin()
	w.taskBegin()
	w.block() // a waits for them

	w.Park() // both of a's children park
	w.Park()
	if flushes != 0 {
		t.Fatal("dispatched while c was still running")
	}
	w.Park() // c parks
	if flushes != 1 {
		t.Fatalf("flushes = %d, want 1 once nothing is left running", flushes)
	}
}
