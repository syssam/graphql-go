package a

// OperationContext stands in for the real one; the analyzer is pointed at
// this package by the test.
type OperationContext struct{ values map[any]any }

func (oc *OperationContext) Get(key any) (any, bool)       { v, ok := oc.values[key]; return v, ok }
func (oc *OperationContext) Set(key, value any)            { oc.values[key] = value }
func (oc *OperationContext) GetOrSet(k, v any) (any, bool) { return v, false }

type scope struct{ n int }

// bad is the shape that cost a silent N+1: check, then act.
func bad(oc *OperationContext, key any) *scope {
	if v, ok := oc.Get(key); ok {
		return v.(*scope)
	}
	s := &scope{}
	oc.Set(key, s) // want `Get\(key\) followed by Set on the same key`
	return s
}

// good uses the atomic pair.
func good(oc *OperationContext, key any) *scope {
	s := &scope{}
	if actual, loaded := oc.GetOrSet(key, s); loaded {
		return actual.(*scope)
	}
	return s
}

// unrelated touches two different keys, which is not the pattern.
func unrelated(oc *OperationContext) {
	oc.Get("read")
	oc.Set("write", 1)
}

// setThenGet writes first and reads back later, which is ordinary use and
// must not be reported: check-then-act requires the Get to come first.
func setThenGet(oc *OperationContext) any {
	oc.Set("seen", 1)
	v, _ := oc.Get("seen")
	return v
}
