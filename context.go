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

	mu     sync.Mutex
	values map[any]any
}

// OperationStats records timing and cache information for observability.
type OperationStats struct {
	Start    time.Time
	CacheHit bool
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

// Complexity returns the static field count of the compiled plan.
func (oc *OperationContext) Complexity() int {
	if oc.plan == nil {
		return 0
	}
	return oc.plan.complexity
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
