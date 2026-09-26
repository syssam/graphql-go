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

	// hub points at wave for an operation the executor started, and stays nil
	// for an OperationContext built by hand, which is the distinction Waves
	// documents. The coordinator is held by value because both are allocated
	// together and always live as long as each other: 160 + 64 bytes in two
	// allocations became 224 in one, the same size class either way.
	hub  *WaveCoordinator
	wave WaveCoordinator

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
//
// Use [OperationContext.GetOrSet] to install request-scoped state. Get
// followed by Set is a check-then-act pair, and sibling resolvers running
// concurrently reach their first call together: each one then sees no value,
// installs its own, and state meant to be shared is silently duplicated. For a
// DataLoader that means the pending queue and the cache split and batching
// degrades to N+1, with no error and no race report -- the repository ships an
// analyzer (lint/cmd/gqlvet) for that pair because -race does not find it.
// Set is for a value the caller knows it is the only writer of.
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

// AuthShape returns what this operation touches, or nil when it touches
// nothing that declares an authorization requirement.
func (oc *OperationContext) AuthShape() *AuthShape {
	if oc.plan == nil {
		return nil
	}
	return oc.plan.shape
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
	return queryCostOf(oc.plan.sel, costWalk{vars: oc.Variables, cfg: QueryCost{DefaultListSize: 1}})
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
// to the context passed to Resolve and ResolveArgs functions only: a Field or
// FieldArgs accessor takes no context, so nothing is attached for one, and a
// FieldInterceptor must use the FieldContext it is handed as a parameter
// rather than FieldFrom.
type FieldContext struct {
	Field  *ast.FieldDefinition
	Object *ast.Definition
	Args   any
	Parent any

	field      *planField
	pathParent *pathNode
	alias      string
}

// Path returns the response path of the field. The node is built here rather
// than when the FieldContext is, because most fields are never asked for it.
func (fc *FieldContext) Path() Path {
	n := pathNode{parent: fc.pathParent, key: fc.alias}
	return n.materialize()
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

// fieldValueCtx carries a FieldContext by value so a resolver field pays one
// allocation for both, where context.WithValue would allocate the wrapper
// separately from the FieldContext it points at.
type fieldValueCtx struct {
	context.Context
	fc FieldContext
}

func (c *fieldValueCtx) Value(key any) any {
	if _, ok := key.(fieldCtxKey); ok {
		return &c.fc
	}
	return c.Context.Value(key)
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
	// ArgumentMap answers both cases.
	Args any

	field *planField
}

// Selection returns the sub-selection requested beneath the field, empty for
// a leaf. Two aliases of one field are two SelectedFields, each with its own
// sub-selection, where Sub returns only the first.
func (f SelectedField) Selection() Selection {
	if f.field == nil {
		return Selection{}
	}
	return Selection{set: f.field.sub}
}

// ArgumentMap returns the field's argument values as the request gave them,
// before binding: variables substituted from vars, defaults applied, and an
// absent argument without a default left out. A number is a json.Number
// whether it was written in the query or sent as a variable, because the
// executor decodes variables with precision kept. It is nil for a field with
// no arguments.
//
// It is for code that cannot know the field's argument struct, such as an ORM
// planning which rows the selection will read. vars is the operation's
// variables, OperationFrom(ctx).Variables; the plan is shared by every
// request with the same query text, so it holds none.
func (f SelectedField) ArgumentMap(vars map[string]any) (map[string]any, error) {
	if f.field == nil || f.field.ast == nil || f.field.ast.Definition == nil {
		return nil, nil
	}
	return fieldArguments(f.field.ast, vars)
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
				if !yield(SelectedField{Name: f.name, Alias: f.alias, Args: f.args, field: f}) {
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

// ForType returns what is selected when the value is of the named object
// type: for an abstract parent, the fields that apply to that type, fragments
// on it and on the interfaces it implements included; empty when typeName is
// not one of the parent's possible types. A concrete parent's selection
// already applies to its one type and is returned as it is.
func (s Selection) ForType(typeName string) Selection {
	if s.set == nil || s.set.byType == nil {
		return s
	}
	return Selection{set: s.set.byType[typeName]}
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
