package graphql

import (
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
)

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
// It must agree exactly with the plan-tree walk it replaces: complexityOf
// (plan.go) and depthOf (limits.go). TestOperationMetricsMatchesPlan checks
// that agreement directly against a compiled plan.
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

	// calls counts walkConcrete invocations. budget, when nonzero, panics
	// once calls exceeds it. operationMetrics leaves budget at zero
	// (unlimited): with the memo intact, calls are bounded by the number of
	// distinct (parent, selection) pairs in the document regardless of
	// nesting depth, so no legitimate operation needs a cap here. A nonzero
	// budget exists only so a test can break the memo on purpose and get a
	// fast, deterministic failure instead of the fanTypes^depth recursion
	// that an unmemoized walk would otherwise run to completion (or to a
	// test-runner timeout) before ever reaching a post-hoc assertion.
	calls  int
	budget int
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
	if w.budget != 0 && w.calls > w.budget {
		panic(fmt.Sprintf("metricWalker: exceeded call budget %d; memo is not deduplicating", w.budget))
	}

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
			// a field whose literal arguments fail to decode, but that path
			// is unreachable here: compilePlan returns that error itself, so
			// complexityOf/depthOf (the spec this walk must match) are never
			// reached for such a document, and neither is this one.
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
