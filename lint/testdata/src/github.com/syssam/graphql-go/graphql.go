// Package graphql stands in for the real one at its real import path, so the
// analyzer is exercised with the ContextType it ships with.
package graphql

type OperationContext struct{ values map[any]any }

func (oc *OperationContext) Get(key any) (any, bool)       { v, ok := oc.values[key]; return v, ok }
func (oc *OperationContext) Set(key, value any)            { oc.values[key] = value }
func (oc *OperationContext) GetOrSet(k, v any) (any, bool) { return v, false }
