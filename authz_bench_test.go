package graphql

import (
	"context"
	"testing"
	"unsafe"
)

// execState and OperationContext sit on size-class boundaries; CLAUDE.md
// records an atomic.Int64 on execState costing +3.2% B/op with its feature
// disabled. These are not assertions about a good number, they are a record
// of the number, so a later change that crosses a boundary is visible.
func TestStructSizes(t *testing.T) {
	t.Logf("execState        = %d bytes", unsafe.Sizeof(execState{}))
	t.Logf("planField        = %d bytes", unsafe.Sizeof(planField{}))

	// OperationContext holds its WaveCoordinator by value, which only pays
	// for itself because 160 + 64 lands exactly on the 224 size class: the
	// separate coordinator cost the same bytes in a second allocation. One
	// more word here crosses to 256 and the embedding starts costing 32
	// bytes a request instead of saving an allocation, so re-measure that
	// trade rather than just updating the number.
	if got := unsafe.Sizeof(OperationContext{}); got != 224 {
		t.Errorf("OperationContext = %d bytes, want 224 (see the comment: the embedded WaveCoordinator depends on it)", got)
	}

	// Unlike the two logged above, this one is asserted: runSubscriptionEvent
	// copies a planField by value once per event (`f := *src`), and a slice
	// field here (argSites was briefly []int32) pushed the struct from 176 to
	// 200 bytes -- a whole extra size class paid on every event. argSites is
	// a count for exactly this reason; see the field's own comment.
	if got := unsafe.Sizeof(planField{}); got != 176 {
		t.Errorf("planField = %d bytes, want 176 (argSites must stay a count, not a slice)", got)
	}

	// argSites is the only word left, so the instance-site flag rides in its
	// high bits. If the mask and the bit ever overlapped, a field with enough
	// arguments would read as instance-guarded and index a site that is not
	// there.
	if argSiteMask&instanceSiteBit != 0 {
		t.Fatal("the instance bit overlaps the argument-site count")
	}

	// Every authorized request allocates a Decision. Holding the argument
	// input table inline ([][]InputKey) took it from 32 to 56 bytes, the
	// 64-byte size class: +32 B/op on BenchmarkExecuteWithAuthorizer for a
	// plan with no argument site at all. The table sits behind src instead.
	if got := unsafe.Sizeof(Decision{}); got != 32 {
		t.Errorf("Decision = %d bytes, want 32 (argument input must stay behind a pointer)", got)
	}
}

func BenchmarkExecuteNoAuthorizer(b *testing.B) {
	s := shapeSchema(b)
	e := NewExecutor(s)
	benchRun(b, e, `{ me { id salary } open }`)
}

func BenchmarkExecuteWithAuthorizer(b *testing.B) {
	s := shapeSchema(b)
	// held is built once, outside the per-call closure: ScopeAuthorizer only
	// reads it (Satisfied), never mutates it, so a fresh map on every
	// Authorize call would be the harness's own allocation counted as the
	// authorizer's cost, not the feature's.
	held := map[string]bool{"pay:read": true}
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return held })))
	benchRun(b, e, `{ me { id salary } open }`)
}

func benchRun(b *testing.B, e *Executor, q string) {
	b.ReportAllocs()
	ctx := context.Background()
	req := &Request{Query: q}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Execute(ctx, req).Release()
	}
}
