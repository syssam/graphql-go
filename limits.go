package graphql

import (
	"encoding/json"
)

// QueryCost is a Shopify / GitHub-style cost model computed from the
// compiled plan and variable values, before resolvers run.
//
// A field costs FieldWeight[Type.field] (default 1) plus the cost of its
// sub-selection multiplied by the list size. List size is the first
// matching ListArguments value (default "first", "last"), or
// DefaultListSize when the field returns a list and neither argument is
// present. __typename is free.
type QueryCost struct {
	// Max is the maximum allowed cost. Zero means unlimited.
	Max int
	// DefaultListSize is used when a list field has no first/last.
	// Zero is treated as 10.
	DefaultListSize int
	// ListArguments names the arguments that bound a connection or list.
	// Empty means first, last.
	ListArguments []string
	// FieldWeight overrides the default weight of 1 for a schema
	// coordinate such as "Query.search".
	FieldWeight map[string]int
	// Report writes extensions.cost on every response, including
	// rejected ones.
	Report bool
	// Actual also reports actualQueryCost, summed from the fields that were
	// really resolved rather than from assumed list sizes. Requested cost has
	// to guess how long a list will be; actual cost counts it, so the two
	// together say whether DefaultListSize is set anywhere near reality.
	//
	// It costs one integer add per resolved field on the write path, which is
	// why it is separate from Report rather than implied by it.
	Actual bool
}

func (c QueryCost) listArgs() []string {
	if len(c.ListArguments) == 0 {
		return []string{"first", "last"}
	}
	return c.ListArguments
}

func (c QueryCost) defaultList() int {
	if c.DefaultListSize <= 0 {
		return 10
	}
	return c.DefaultListSize
}

func (c QueryCost) weight(coord string) int {
	if c.FieldWeight != nil {
		if w, ok := c.FieldWeight[coord]; ok {
			return w
		}
	}
	return 1
}

// WithMaxComplexity rejects operations whose static field count (aliases
// included) exceeds n. Zero, the default, means unlimited.
func WithMaxComplexity(n int) ExecutorOption {
	return func(e *Executor) { e.maxComplexity = n }
}

// WithMaxDepth rejects operations whose selection nesting exceeds n.
// Zero, the default, means unlimited.
func WithMaxDepth(n int) ExecutorOption {
	return func(e *Executor) { e.maxDepth = n }
}

// WithQueryCost enables Shopify-style query cost. When c.Max > 0,
// operations above the cap are rejected before execution.
func WithQueryCost(c QueryCost) ExecutorOption {
	return func(e *Executor) {
		cp := c
		e.cost = &cp
	}
}

func (e *Executor) rejectIfOverLimit(oc *OperationContext) *Error {
	if oc.plan == nil {
		return nil
	}
	if e.maxComplexity > 0 && oc.plan.complexity > e.maxComplexity {
		return Errorf("query exceeds complexity limit: %d > %d", oc.plan.complexity, e.maxComplexity).
			WithCode(CodeTooComplex).
			WithExtension("complexity", oc.plan.complexity).
			WithExtension("maxComplexity", e.maxComplexity)
	}
	if e.maxDepth > 0 && oc.plan.depth > e.maxDepth {
		return Errorf("query exceeds maximum depth: %d > %d", oc.plan.depth, e.maxDepth).
			WithCode(CodeMaxDepth).
			WithExtension("depth", oc.plan.depth).
			WithExtension("maxDepth", e.maxDepth)
	}
	if e.cost != nil && e.cost.Max > 0 {
		n := oc.ensureCost(*e.cost)
		if n > e.cost.Max {
			return Errorf("query exceeds cost limit: %d > %d", n, e.cost.Max).
				WithCode(CodeTooComplex).
				WithExtension("requestedQueryCost", n).
				WithExtension("maxQueryCost", e.cost.Max)
		}
	}
	return nil
}

func (e *Executor) attachCost(oc *OperationContext, resp *Response) {
	if e.cost == nil || !e.cost.Report || oc == nil || oc.plan == nil {
		return
	}
	n := oc.ensureCost(*e.cost)
	payload := map[string]any{"requestedQueryCost": n}
	if e.cost.Max > 0 {
		payload["maxQueryCost"] = e.cost.Max
	}
	// An operation rejected before execution resolved nothing, so reporting a
	// zero would read as "this query was free" rather than "it never ran".
	if e.cost.Actual && oc.actualOK {
		payload["actualQueryCost"] = int(oc.actualCost)
	}
	if resp.Extensions == nil {
		resp.Extensions = map[string]any{}
	}
	resp.Extensions["cost"] = payload
}

func (oc *OperationContext) ensureCost(cfg QueryCost) int {
	if oc.costOK {
		return oc.costValue
	}
	oc.costValue = queryCostOf(oc.plan.sel, oc.Variables, cfg)
	oc.costOK = true
	return oc.costValue
}

func queryCostOf(sel *selectionSet, vars map[string]any, cfg QueryCost) int {
	if sel == nil {
		return 0
	}
	sum := func(fields []*planField) int {
		n := 0
		for _, f := range fields {
			n += fieldCost(f, vars, cfg)
		}
		return n
	}
	if sel.byType == nil {
		return sum(sel.fields)
	}
	most := 0
	for _, concrete := range sel.byType {
		most = max(most, sum(concrete.fields))
	}
	return most
}

func fieldCost(f *planField, vars map[string]any, cfg QueryCost) int {
	if f.kind == fieldTypename {
		return 0
	}
	weight := 1
	if f.def != nil && f.def.object != nil {
		weight = cfg.weight(coordinate(f.def.object.name, f.def.name))
	}
	child := 0
	if f.sub != nil {
		child = queryCostOf(f.sub, vars, cfg)
	}
	mult := 1
	if f.def != nil && f.def.typ != nil && f.def.typ.Elem != nil {
		mult = listMultiplier(f, vars, cfg)
	}
	return weight + child*mult
}

func listMultiplier(f *planField, vars map[string]any, cfg QueryCost) int {
	def := cfg.defaultList()
	if f.ast == nil {
		return def
	}
	for _, name := range cfg.listArgs() {
		a := f.ast.Arguments.ForName(name)
		if a == nil {
			continue
		}
		raw, err := a.Value.Value(vars)
		if err != nil {
			continue
		}
		if n, ok := asCostInt(raw); ok && n > 0 {
			return n
		}
	}
	return def
}

func asCostInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func depthOf(sel *selectionSet) int {
	if sel == nil {
		return 0
	}
	walk := func(fields []*planField) int {
		d := 0
		for _, f := range fields {
			fd := 1
			if f.sub != nil {
				fd += depthOf(f.sub)
			}
			d = max(d, fd)
		}
		return d
	}
	if sel.byType == nil {
		return walk(sel.fields)
	}
	d := 0
	for _, c := range sel.byType {
		d = max(d, walk(c.fields))
	}
	return d
}
