package graphql

import "github.com/vektah/gqlparser/v2/ast"

// planMetrics is an operation's static complexity and nesting depth.
type planMetrics struct {
	complexity int
	depth      int
}

// operationMetrics computes an operation's complexity and depth from the
// document, before any plan exists. Computing them from the finished plan, as
// this once did, means the limits can only report on an expansion that has
// already happened rather than prevent it.
//
// It must agree exactly with the plan-tree walk it replaces; that walk is
// retained as planMetricsOracle in plan_metrics_oracle_test.go and the two are
// compared by TestOperationMetricsMatchesPlan and FuzzOperationMetrics.
func operationMetrics(s *Schema, doc *ast.QueryDocument, op *ast.OperationDefinition, cond map[string]bool) planMetrics {
	root := s.rootFor(op.Operation)
	if root == nil {
		return planMetrics{}
	}
	// memo is left nil deliberately: selFingerprint/nodeNum touch only
	// nodeID, and metricWalker never calls compileSelection (the only method
	// that reads or writes memo), so the nil map is never touched. Building
	// the real memo here would cost an allocation this walk never uses.
	w := &metricWalker{
		c:    &compiler{s: s, doc: doc, cond: cond, nodeID: make(map[ast.Selection]int32)},
		memo: make(map[selKey]planMetrics),
	}
	return w.walk(root, nil, op.SelectionSet)
}

// metricWalker mirrors compileSelection without building a plan. It carries
// the same memo, because a guard that expands N^depth in order to decide
// whether to refuse expanding N^depth has refused nothing.
type metricWalker struct {
	c    *compiler
	memo map[selKey]planMetrics
}

func (w *metricWalker) walk(obj *objectType, abs *abstractType, sels ast.SelectionSet) planMetrics {
	key := selKey{parent: any(obj), sels: w.c.selFingerprint(sels)}
	if abs != nil {
		key.parent = any(abs)
	}
	if m, ok := w.memo[key]; ok {
		return m
	}

	var m planMetrics
	if abs == nil {
		m = w.walkConcrete(obj, sels)
	} else {
		// An abstract parent contributes the largest of its possible types,
		// matching how the plan-tree walk takes a max over byType.
		for _, concrete := range abs.possible {
			c := w.walkConcrete(concrete, sels)
			m.complexity = max(m.complexity, c.complexity)
			m.depth = max(m.depth, c.depth)
		}
	}
	w.memo[key] = m
	return m
}

func (w *metricWalker) walkConcrete(obj *objectType, sels ast.SelectionSet) planMetrics {
	var groups []*fieldGroup
	index := make(map[string]*fieldGroup)
	visited := make(map[string]bool)
	w.c.collectInto(obj, sels, &groups, index, visited)

	var m planMetrics
	for _, g := range groups {
		first := g.fields[0]
		if first.Name == "__typename" {
			m.complexity++
			m.depth = max(m.depth, 1)
			continue
		}
		fd := obj.fields[first.Name]
		if fd == nil {
			// buildField drops this group and reports an error, so it
			// contributes nothing to either number.
			continue
		}
		if fd.leaf {
			m.complexity++
			m.depth = max(m.depth, 1)
			continue
		}

		var merged ast.SelectionSet
		if len(g.fields) == 1 {
			merged = first.SelectionSet
		} else {
			for _, f := range g.fields {
				merged = append(merged, f.SelectionSet...)
			}
		}
		named := w.c.s.ast.Types[fd.typ.Name()]
		var sub planMetrics
		if named.Kind == ast.Object {
			sub = w.walk(w.c.s.objects[named.Name], nil, merged)
		} else {
			sub = w.walk(nil, w.c.s.abstracts[named.Name], merged)
		}
		m.complexity += 1 + sub.complexity
		m.depth = max(m.depth, 1+sub.depth)
	}
	return m
}
