package graphql

import "context"

// FieldInfo identifies a field to an observer. It is a value, and its path is
// materialized only if asked for, so observing a field costs nothing until the
// observer itself spends something.
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
type FieldObserver interface {
	// BeginField runs before the field. The context it returns is the one the
	// field and its children run under, so an observer that starts a span
	// returns the context carrying it.
	BeginField(ctx context.Context, f FieldInfo) context.Context

	// EndField runs after the field, with the context BeginField returned.
	EndField(ctx context.Context, f FieldInfo, err error)
}

// WithFieldObserver registers field observers. The first is outermost, as with
// the interceptor options.
func WithFieldObserver(o ...FieldObserver) ExecutorOption {
	return func(e *Executor) { e.fieldObservers = append(e.fieldObservers, o...) }
}
