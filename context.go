package graphql

import (
	"context"
	"iter"
	"sync"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
)

// OperationContext describes the operation being executed. It is available
// from any context derived from the request through OperationFrom.
type OperationContext struct {
	Operation     *ast.OperationDefinition
	Doc           *ast.QueryDocument
	RawQuery      string
	OperationName string
	Variables     map[string]any
	Stats         OperationStats

	plan  *plan
	entry *docEntry
	hub   *WaveCoordinator

	// event is set only while executing one event of a subscription, and is
	// what routes the operation chain to the per-event writer.
	event *subEvent

	// These four pack into the two words costOK and costValue used to take on
	// their own: two flags, a 32-bit measured cost beside them, then the
	// requested cost. Laid out any other way the struct crosses a size class
	// and every request pays 16 bytes for a feature most never enable.
	costOK   bool
	actualOK bool
	// actualCost is summed during execution; actualOK distinguishes a query
	// that resolved nothing from one that never ran.
	actualCost int32
	costValue  int

	mu         sync.Mutex
	values     map[any]any
	extensions map[string]any
}

// OperationStats records timing and cache information for observability.
type OperationStats struct {
	Start    time.Time
	CacheHit bool
	// PlanUncacheable reports that this document references more Boolean
	// @skip/@include variables than the variant cache holds, so its plan is
	// compiled on every request and CacheHit can never become true. It is
	// reported separately because a false CacheHit alone is indistinguishable
	// from the first request for a perfectly cacheable document.
	PlanUncacheable bool
}

// Set stores an arbitrary value scoped to the operation, for use by
// interceptors and extensions.
func (oc *OperationContext) Set(key, value any) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.values == nil {
		oc.values = make(map[any]any)
	}
	oc.values[key] = value
}

// Get retrieves a value stored with Set.
func (oc *OperationContext) Get(key any) (any, bool) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	v, ok := oc.values[key]
	return v, ok
}

// GetOrSet returns the existing value for key, or stores and returns value
// when the key is absent; loaded reports which happened. It is the atomic
// form of Get followed by Set.
//
// Request-scoped extensions must use it rather than Get-then-Set: concurrent
// resolvers reach their first Load at the same time, and a check-then-act
// pair lets every one of them install its own state, so only the last write
// survives while the others keep using orphaned copies.
func (oc *OperationContext) GetOrSet(key, value any) (actual any, loaded bool) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if v, ok := oc.values[key]; ok {
		return v, true
	}
	if oc.values == nil {
		oc.values = make(map[any]any)
	}
	oc.values[key] = value
	return value, false
}

// ActualCost returns the cost summed from the fields this operation really
// resolved, and whether it was measured. It is measured only when the executor
// was built with a QueryCost whose Actual is set, and only after execution.
func (oc *OperationContext) ActualCost() (int, bool) {
	return int(oc.actualCost), oc.actualOK
}

// Complexity returns the static field count computed by operationMetrics
// over the document, copied onto the plan at compile time.
func (oc *OperationContext) Complexity() int {
	if oc.plan == nil {
		return 0
	}
	return oc.plan.complexity
}

// Depth returns the maximum selection nesting computed by operationMetrics
// over the document, copied onto the plan at compile time.
func (oc *OperationContext) Depth() int {
	if oc.plan == nil {
		return 0
	}
	return oc.plan.depth
}

// Cost returns the Shopify-style query cost. Without WithQueryCost it uses
// a default list size of 1 so callers can still observe a number.
func (oc *OperationContext) Cost() int {
	if oc.plan == nil {
		return 0
	}
	if oc.costOK {
		return oc.costValue
	}
	return queryCostOf(oc.plan.sel, oc.Variables, QueryCost{DefaultListSize: 1})
}

// SetExtension records a response-level extension (Netflix / Apollo
// tracing, GitHub rate-limit metadata). Values are copied onto the
// Response after the operation completes.
func (oc *OperationContext) SetExtension(key string, value any) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.extensions == nil {
		oc.extensions = make(map[string]any, 1)
	}
	oc.extensions[key] = value
}

// FieldContext describes the field a resolver is executing. It is attached
// to the context passed to Resolve and ResolveArgs functions.
type FieldContext struct {
	Field  *ast.FieldDefinition
	Object *ast.Definition
	Args   any
	Parent any

	field *planField
	path  *pathNode
}

// Path returns the response path of the field.
func (fc *FieldContext) Path() Path {
	return fc.path.materialize()
}

// Selection returns the sub-selection requested beneath the field.
func (fc *FieldContext) Selection() Selection {
	return Selection{set: fc.field.sub}
}

type operationCtxKey struct{}
type fieldCtxKey struct{}

func withOperation(ctx context.Context, oc *OperationContext) context.Context {
	return context.WithValue(ctx, operationCtxKey{}, oc)
}

func withField(ctx context.Context, fc *FieldContext) context.Context {
	return context.WithValue(ctx, fieldCtxKey{}, fc)
}

// OperationFrom returns the OperationContext, or nil outside a request.
func OperationFrom(ctx context.Context) *OperationContext {
	oc, _ := ctx.Value(operationCtxKey{}).(*OperationContext)
	return oc
}

// FieldFrom returns the FieldContext, or nil when ctx does not belong to a
// resolver invocation.
func FieldFrom(ctx context.Context) *FieldContext {
	fc, _ := ctx.Value(fieldCtxKey{}).(*FieldContext)
	return fc
}

// PathFrom returns the response path of the executing resolver, or nil.
func PathFrom(ctx context.Context) Path {
	fc := FieldFrom(ctx)
	if fc == nil {
		return nil
	}
	return fc.Path()
}

// SelectionFrom returns the sub-selection of the executing resolver. Outside
// a resolver it returns an empty Selection.
func SelectionFrom(ctx context.Context) Selection {
	fc := FieldFrom(ctx)
	if fc == nil || fc.field == nil {
		return Selection{}
	}
	return fc.Selection()
}

// Selection is a read-only view of the fields requested beneath a field. It
// lets resolvers fetch only what the client asked for.
type Selection struct {
	set *selectionSet
}

// SelectedField describes one requested field.
type SelectedField struct {
	Name  string
	Alias string
	// Args holds the decoded argument struct pointer when the arguments are
	// literal, or nil when they depend on variables or the field has none.
	Args any
}

// IsEmpty reports whether nothing is selected.
func (s Selection) IsEmpty() bool {
	return s.set == nil || (len(s.set.fields) == 0 && len(s.set.byType) == 0)
}

// Has reports whether a field with the given name is selected, on any
// possible type when the parent is abstract.
func (s Selection) Has(name string) bool {
	for f := range s.Fields() {
		if f.Name == name {
			return true
		}
	}
	return false
}

// Fields iterates over the selected fields. For abstract parents, fields
// from all possible types are visited once per response key.
func (s Selection) Fields() iter.Seq[SelectedField] {
	return func(yield func(SelectedField) bool) {
		if s.set == nil {
			return
		}
		emit := func(fields []*planField, seen map[string]bool) bool {
			for _, f := range fields {
				if seen != nil {
					if seen[f.alias] {
						continue
					}
					seen[f.alias] = true
				}
				if !yield(SelectedField{Name: f.name, Alias: f.alias, Args: f.args}) {
					return false
				}
			}
			return true
		}
		if s.set.byType == nil {
			emit(s.set.fields, nil)
			return
		}
		seen := make(map[string]bool)
		for _, concrete := range s.set.byType {
			if !emit(concrete.fields, seen) {
				return
			}
		}
	}
}

// Collect builds a slice from the selected fields. It is a generic method
// (Go 1.27) so callers write sel.Collect(func(f SelectedField) string { ... }).
func (s Selection) Collect[T any](fn func(SelectedField) T) []T {
	var out []T
	for f := range s.Fields() {
		out = append(out, fn(f))
	}
	return out
}

// Sub returns the selection beneath the named field. The second result is
// false when the field is not selected or is a leaf.
func (s Selection) Sub(name string) (Selection, bool) {
	if s.set == nil {
		return Selection{}, false
	}
	find := func(fields []*planField) (Selection, bool) {
		for _, f := range fields {
			if f.name == name && f.sub != nil {
				return Selection{set: f.sub}, true
			}
		}
		return Selection{}, false
	}
	if s.set.byType == nil {
		return find(s.set.fields)
	}
	for _, concrete := range s.set.byType {
		if sub, ok := find(concrete.fields); ok {
			return sub, true
		}
	}
	return Selection{}, false
}
