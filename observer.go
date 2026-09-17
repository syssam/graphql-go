package graphql

import "context"

// FieldInfo identifies a field to an observer. It is a value, and its path is
// materialized only if asked for, so the engine allocates nothing to observe a
// field until the observer itself spends something. It is not free in time:
// each observed field still pays two interface calls and a defer.
//
// It deliberately carries neither the arguments nor the parent value: those
// are what a FieldContext is for, and wanting them means wanting a
// FieldInterceptor.
type FieldInfo struct {
	Object string
	Field  string
	Alias  string

	pathParent *pathNode
}

// Path returns the response path of the field.
func (f FieldInfo) Path() Path {
	n := pathNode{parent: f.pathParent, key: f.Alias}
	return n.materialize()
}

// FieldObserver watches every field, pure ones included, without seeing its
// value. Not seeing the value is what lets the engine leave each field on its
// typed write path: a FieldInterceptor, which may replace a result, cannot.
//
// A field that never runs is never observed: __typename, a field whose
// arguments fail to decode, and a field skipped because the request context
// was already cancelled.
//
// Sibling resolver fields are scheduled concurrently, so both methods may be
// called from several goroutines at once; an observer that holds state of its
// own must synchronise it.
//
// A panic inside BeginField or EndField is not recovered by WithRecover:
// BeginField runs before the field's recovery is in place, and EndField after
// it has finished, so that EndField can see a recovered panic as err.
type FieldObserver interface {
	// BeginField runs before the field. The context it returns is the one the
	// field's own resolver runs under, so a span the resolver starts nests
	// beneath one the observer started. It is not the context of the field's
	// sub-selection: children run under the context the field itself
	// received.
	BeginField(ctx context.Context, f FieldInfo) context.Context

	// EndField runs when the field's resolver returns, with the context this
	// observer's BeginField returned. For a composite field that is before its
	// sub-selection is written, so a span ended here covers the resolver and
	// not the children.
	EndField(ctx context.Context, f FieldInfo, err error)
}

// WithFieldObserver registers field observers. The first is outermost, as with
// the interceptor options.
func WithFieldObserver(o ...FieldObserver) ExecutorOption {
	return func(e *Executor) { e.fieldObservers = append(e.fieldObservers, o...) }
}
