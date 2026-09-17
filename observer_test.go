package graphql

import (
	"context"
	"fmt"
	"sync"
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
	if testing.Short() {
		t.Skip("runs two benchmarks")
	}
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

// TestFieldObserverSeesPanicAsError pins the defer ordering in callLeaf: the
// observer's EndField must run after panic recovery has converted the panic
// into the named error return, not before. Registering the observer's defer
// after the recovery defer would make EndField run first (defers are LIFO)
// and see err == nil for a field that panicked -- which would make ext/otel
// record a crashed field as a successful span. The fixture's "panics" field
// returns Int, so fd.leaf is true regardless of it being Resolve-bound, and
// it dispatches through callLeaf -- see
// TestFieldObserverSeesPanicAsErrorForCompositeResolve for the callResolve
// side of the same ordering.
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

type panicComposite struct{ Name string }

// newPanicResolveSchema is a dedicated minimal schema rather than an addition
// to the shared fixture: a Resolve field with a composite return type that
// always panics would be a landmine for every other test that walks the
// fixture schema looking for coverage.
func newPanicResolveSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(`
		type Boom { name: String! }
		type Query { boom: Boom }
	`),
		Object[panicComposite]("Boom",
			Field("name", func(b *panicComposite) string { return b.Name }),
		),
		Query(
			Resolve("boom", func(context.Context, Root) (*panicComposite, error) {
				panic("kaboom-composite")
			}),
		),
	)
	if err != nil {
		t.Fatalf("panic-resolve schema: %v", err)
	}
	return s
}

// TestFieldObserverSeesPanicAsErrorForCompositeResolve is
// TestFieldObserverSeesPanicAsError's counterpart for callResolve: "boom"
// returns a composite type, so it dispatches through callResolve rather than
// callLeaf, and that function has its own copy of the observer-before-
// recovery ordering that needs its own regression coverage.
func TestFieldObserverSeesPanicAsErrorForCompositeResolve(t *testing.T) {
	s := newPanicResolveSchema(t)
	o := &panicObserver{}
	e := NewExecutor(s, WithFieldObserver(o), WithRecover(true))
	run(t, e, `{ boom { name } }`, "")

	err, ok := o.errByField["Query.boom"]
	if !ok {
		t.Fatal("observer never saw Query.boom")
	}
	if err == nil {
		t.Fatal("EndField saw a nil error for a composite field that panicked")
	}
}

// taggingObserver stores its own tag under a key every instance shares, so a
// context handed to the wrong observer's EndField reads back another's tag.
type taggingObserver struct {
	tag string
	mu  sync.Mutex
	bad []string
}

type sharedObsKey struct{}

func (o *taggingObserver) BeginField(ctx context.Context, f FieldInfo) context.Context {
	return context.WithValue(ctx, sharedObsKey{}, o.tag)
}

func (o *taggingObserver) EndField(ctx context.Context, f FieldInfo, err error) {
	if got, _ := ctx.Value(sharedObsKey{}).(string); got != o.tag {
		o.mu.Lock()
		o.bad = append(o.bad, f.Object+"."+f.Field+" saw "+got)
		o.mu.Unlock()
	}
}

// TestFieldObserversEachGetTheirOwnContext covers more than one observer:
// each EndField must receive the context its own BeginField returned, not the
// innermost observer's. Handing every observer the final context made ext/otel
// end the inner span twice and never end the outer one.
func TestFieldObserversEachGetTheirOwnContext(t *testing.T) {
	// Two is the case that regressed; nine outgrows any small fixed store the
	// engine keeps the contexts in, so both storage paths are covered.
	for _, n := range []int{2, 9} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var obs []*taggingObserver
			var opts []FieldObserver
			for i := range n {
				o := &taggingObserver{tag: fmt.Sprint("observer-", i)}
				obs = append(obs, o)
				opts = append(opts, o)
			}
			_, e := newFixtureExecutor(t, WithFieldObserver(opts...))
			run(t, e, `{ users { id name } }`, "")

			for _, o := range obs {
				if len(o.bad) > 0 {
					t.Errorf("observer %q got another observer's context: %v", o.tag, o.bad)
				}
			}
		})
	}
}
