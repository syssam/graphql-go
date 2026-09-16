package graphql

import (
	"context"
	"testing"
)

type recordingObserver struct {
	begun []string
	ended []string
	errs  []error
	depth []any
}

type obsKey struct{}

func (o *recordingObserver) BeginField(ctx context.Context, f FieldInfo) context.Context {
	o.begun = append(o.begun, f.Object+"."+f.Field)
	return context.WithValue(ctx, obsKey{}, f.Object+"."+f.Field)
}

func (o *recordingObserver) EndField(ctx context.Context, f FieldInfo, err error) {
	o.ended = append(o.ended, f.Object+"."+f.Field)
	o.errs = append(o.errs, err)
	o.depth = append(o.depth, ctx.Value(obsKey{}))
}

// TestFieldObserverSeesPureFields is the point of the whole change: a pure
// field is observed without an interceptor, and therefore without leaving its
// typed write path.
func TestFieldObserverSeesPureFields(t *testing.T) {
	o := &recordingObserver{}
	_, e := newFixtureExecutor(t, WithFieldObserver(o))
	run(t, e, `{ users { id } }`, "")

	if len(o.begun) == 0 {
		t.Fatal("observer never ran")
	}
	var sawPure bool
	for _, name := range o.begun {
		if name == "User.id" {
			sawPure = true
		}
	}
	if !sawPure {
		t.Errorf("observer never saw the pure field User.id; saw %v", o.begun)
	}
	if len(o.ended) != len(o.begun) {
		t.Errorf("%d BeginField against %d EndField", len(o.begun), len(o.ended))
	}
}

// TestFieldObserverEndSeesBeginContext pins the contract that lets an observer
// keep state without storing it anywhere: EndField gets the context BeginField
// returned, which is where tracer.Start already puts its span.
func TestFieldObserverEndSeesBeginContext(t *testing.T) {
	o := &recordingObserver{}
	_, e := newFixtureExecutor(t, WithFieldObserver(o))
	run(t, e, `{ users { id } }`, "")

	for i, v := range o.depth {
		if v != o.ended[i] {
			t.Errorf("EndField %d saw context value %v, want %q", i, v, o.ended[i])
		}
	}
}

// TestFieldObserverKeepsTypedWritePath is the structural assertion. Timing
// cannot tell the two paths apart reliably on this machine; an allocation
// bound can, because the type-erased path allocates per leaf and the typed one
// does not.
func TestFieldObserverKeepsTypedWritePath(t *testing.T) {
	bare := testing.Benchmark(func(b *testing.B) { benchFieldPath(b) })
	obs := testing.Benchmark(func(b *testing.B) {
		benchFieldPath(b, WithFieldObserver(&countingObserver{}))
	})
	// A countingObserver allocates nothing of its own, so any gap is the
	// engine's. The type-erased path costs tens of allocations on this query;
	// a handful of slack absorbs observer bookkeeping without absorbing that.
	if got, want := obs.AllocsPerOp(), bare.AllocsPerOp()+8; got > want {
		t.Fatalf("observer path allocates %d/op against %d/op bare; want at most %d",
			got, bare.AllocsPerOp(), want)
	}
	t.Logf("bare=%d observer=%d gap=%d", bare.AllocsPerOp(), obs.AllocsPerOp(), obs.AllocsPerOp()-bare.AllocsPerOp())
}

type countingObserver struct{ n int }

func (c *countingObserver) BeginField(ctx context.Context, f FieldInfo) context.Context {
	c.n++
	return ctx
}
func (c *countingObserver) EndField(context.Context, FieldInfo, error) {}

// panicObserver records the error EndField receives, keyed by field, so the
// panic-recovery ordering test can check a specific field's outcome without
// depending on visit order.
type panicObserver struct {
	errByField map[string]error
}

func (o *panicObserver) BeginField(ctx context.Context, f FieldInfo) context.Context {
	return ctx
}

func (o *panicObserver) EndField(ctx context.Context, f FieldInfo, err error) {
	if o.errByField == nil {
		o.errByField = make(map[string]error)
	}
	o.errByField[f.Object+"."+f.Field] = err
}

// TestFieldObserverSeesPanicAsError pins the defer ordering in callLeaf and
// callResolve: the observer's EndField must run after panic recovery has
// converted the panic into the named error return, not before. Registering
// the observer's defer after the recovery defer would make EndField run
// first (defers are LIFO) and see err == nil for a field that panicked --
// which would make ext/otel record a crashed field as a successful span.
func TestFieldObserverSeesPanicAsError(t *testing.T) {
	o := &panicObserver{}
	_, e := newFixtureExecutor(t, WithFieldObserver(o), WithRecover(true))
	run(t, e, `{ panics }`, "")

	err, ok := o.errByField["Query.panics"]
	if !ok {
		t.Fatal("observer never saw Query.panics")
	}
	if err == nil {
		t.Fatal("EndField saw a nil error for a field that panicked")
	}
}
