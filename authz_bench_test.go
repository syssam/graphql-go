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
	t.Logf("OperationContext = %d bytes", unsafe.Sizeof(OperationContext{}))
	t.Logf("planField        = %d bytes", unsafe.Sizeof(planField{}))
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
