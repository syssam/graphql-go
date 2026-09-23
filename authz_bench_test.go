package graphql

import (
	"context"
	"testing"
	"unsafe"
)

// The three hot-path structs sit on size-class boundaries; CLAUDE.md records
// an atomic.Int64 on execState costing +3.2% B/op with its feature disabled.
// All three are asserted rather than logged. execState was logged only, which
// meant the struct CLAUDE.md names first -- "check unsafe.Sizeof before adding
// a field" -- was the one nothing checked: a field pushing it from 64 to 72
// printed the new number and passed.
//
// Crossing a boundary here is allowed, but it is a decision to be taken with
// an interleaved benchstat and written down, not one to be discovered later
// in a log line nobody reads.
func TestStructSizes(t *testing.T) {
	// execState is allocated per request and per concurrent field group. 64
	// is the 64 class exactly; one more word is 80, a quarter more memory on
	// the single hottest allocation in the engine.
	if got := unsafe.Sizeof(execState{}); got != 64 {
		t.Errorf("execState = %d bytes, want 64; crossing the size class is a benchstat decision", got)
	}

	// OperationContext holds its WaveCoordinator by value, which only pays
	// for itself because the two land on one size class: 160 + 64 was exactly
	// 224, and the separate coordinator cost the same bytes in a second
	// allocation. The coordinator is 48 bytes since it keeps three counts for
	// the operation instead of a stack of waves, so 208 is the 208 class. One
	// more word crosses to 224 and costs 16 bytes a request, so re-measure that
	// trade rather than just updating the number.
	if got := unsafe.Sizeof(OperationContext{}); got != 208 {
		t.Errorf("OperationContext = %d bytes, want 208 (see the comment: the embedded WaveCoordinator depends on it)", got)
	}

	// runSubscriptionEvent
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
