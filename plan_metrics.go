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
// It must agree exactly with the plan-tree walk it replaces: planMetricsOracle
// (plan_metrics_oracle_test.go), built from complexityOf and depthOf, the
// same functions this walk once was before this task moved them there.
// TestOperationMetricsMatchesPlan checks that agreement directly against a
// compiled plan.
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

	// calls counts walkConcrete invocations. It is plain bookkeeping with no
	// effect on the result: operationMetrics never reads it, and nothing
	// here acts on its value. It exists so a test can assert a structural
	// bound on it directly, rather than the walk enforcing one itself — a
	// pre-plan cost check is exactly the kind of thing Task 5 wires this
	// walker into, and a hard cap living here would need to turn a "this
	// document is merely large" case into an error, which is a product
	// decision this file should not make unilaterally.
	calls int
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
		// matching how the plan-tree walk takes a max over byType. Recursing
		// through walk (not walkConcrete directly) routes each concrete type
		// through the same (concrete, sels) memo level compileSelection
		// uses, so a concrete type reached from two different abstract
		// ancestors with the same AST selection is computed once.
		//
		// Ranging over abs.possible (a map) visits concrete types in an
		// unspecified order, unlike compileSelection's sorted iteration
		// (plan.go:203-207). That is fine only because max is commutative;
		// an edit that makes this loop order-sensitive needs the same sort.
		for _, concrete := range abs.possible {
			c := w.walk(concrete, nil, sels)
			m.complexity = max(m.complexity, c.complexity)
			m.depth = max(m.depth, c.depth)
		}
	}
	w.memo[key] = m
	return m
}

func (w *metricWalker) walkConcrete(obj *objectType, sels ast.SelectionSet) planMetrics {
	w.calls++

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
			// contributes nothing to either number. buildField can also drop
			// a field whose literal arguments fail to decode, but that
			// disagreement is unobservable: operationMetrics runs before
			// compilePlan and may reject the operation on depth or
			// complexity before compilePlan ever produces that argument
			// error, so there is no document for which the two walks are
			// compared and found to differ.
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
