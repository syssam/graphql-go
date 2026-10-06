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
	return limitedMetrics(s, doc, op, cond, 0, 0)
}

// limitedMetrics is operationMetrics for a caller that only needs to know
// whether the operation is within maxComplexity and maxDepth, zero for no
// limit. Within them the result is exact. Past either it is exact too unless
// the walk was a large one, and then it is past that limit and no greater
// than the true figure: see metricWalker.settled.
func limitedMetrics(s *Schema, doc *ast.QueryDocument, op *ast.OperationDefinition, cond map[string]bool, maxComplexity, maxDepth int) planMetrics {
	root := s.rootFor(op.Operation)
	if root == nil {
		return planMetrics{}
	}
	// memo is left nil deliberately: selFingerprint/nodeNum touch only
	// nodeID, and metricWalker never calls compileSelection (the only method
	// that reads or writes memo), so the nil map is never touched. Building
	// the real memo here would cost an allocation this walk never uses.
	w := &metricWalker{
		c:             &compiler{s: s, doc: doc, cond: cond, nodeID: make(map[ast.Selection]int32)},
		memo:          make(map[selKey]planMetrics),
		maxComplexity: maxComplexity,
		maxDepth:      maxDepth,
		exact:         metricExactWork,
	}
	return w.walk(root, nil, op.SelectionSet)
}

// metricExactWork is how many fields a walk may collect and still owe an
// exact figure. A document under the default token limit has fewer fields
// than this, so a walk past it is collecting the same ones again -- under
// each possible type of an abstract parent, or a fragment at each place it
// is spread -- and that is the walk a refusal should not have to finish.
const metricExactWork = 1 << 14

// metricWalker mirrors compileSelection without building a plan. It carries
// the same memo, because a guard that expands N^depth in order to decide
// whether to refuse expanding N^depth has refused nothing.
type metricWalker struct {
	c    *compiler
	memo map[selKey]planMetrics

	// maxComplexity and maxDepth are the executor's limits, zero for none,
	// and exact is how many fields the walk collects before being over one
	// of them is reason to stop: see settled.
	maxComplexity int
	maxDepth      int
	exact         int
	collected     int

	// calls counts walkConcrete invocations. It is plain bookkeeping with no
	// effect on the result: operationMetrics never reads it, and nothing
	// here acts on its value. It exists so a test can assert a structural
	// bound on it directly, rather than the walk enforcing one itself — a
	// pre-plan cost check is exactly the kind of thing this walker is wired
	// into, and a hard cap living here would need to turn a "this
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
		// The types are taken in the schema's order, not the map's: the walk
		// may stop part way (settled), and what it reports then must be the
		// same on every request.
		for _, def := range w.c.s.ast.GetPossibleTypes(abs.def) {
			concrete := abs.possible[def.Name]
			if concrete == nil {
				continue
			}
			c := w.walk(concrete, nil, sels)
			m.complexity = max(m.complexity, c.complexity)
			m.depth = max(m.depth, c.depth)
			if w.settled(m) {
				break
			}
		}
	}
	w.memo[key] = m
	return m
}

// settled reports that the walk may stop with m as it stands. A selection's
// metrics only add to those of whatever contains it, so once m is past a
// limit the operation is refused whatever the rest measures, and the rest
// can be most of the work: an interface every entity implements is walked
// once per entity, and a fragment once for each field it is spread under.
// The figure reported is then how far the walk got rather than the whole.
// A small walk is finished anyway, so that an ordinary query over the limit
// is still told by exactly how much.
func (w *metricWalker) settled(m planMetrics) bool {
	if w.collected < w.exact {
		return false
	}
	return (w.maxComplexity > 0 && m.complexity > w.maxComplexity) || (w.maxDepth > 0 && m.depth > w.maxDepth)
}

func (w *metricWalker) walkConcrete(obj *objectType, sels ast.SelectionSet) planMetrics {
	w.calls++

	var groups []*fieldGroup
	index := make(map[string]*fieldGroup)
	visited := make(map[string]bool)
	w.c.collectInto(obj, sels, &groups, index, visited)

	for _, g := range groups {
		w.collected += len(g.fields)
	}

	var m planMetrics
	for _, g := range groups {
		if w.settled(m) {
			break
		}
		first := g.fields[0]
		if first.Name == "__typename" {
			m.complexity = costAdd(m.complexity, 1)
			m.depth = max(m.depth, 1)
			continue
		}
		fd := obj.fields[first.Name]
		if fd == nil {
			// buildField drops this group and reports an error, so it
			// contributes nothing to either number. buildField can also drop
			// a field whose literal arguments fail to decode, but that
			// disagreement is unobservable: compilePlan fails wholesale
			// if any error occurred during compilation,
			// returning nil before a plan exists. When an argument decode error
			// is recorded in c.errs, both comparison sites bail on error checks
			// before any oracle value exists to compare against.
			continue
		}
		if fd.leaf || isIntrospectionRoot(first.Name) {
			m.complexity = costAdd(m.complexity, 1)
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
		// The walk is linear in the document, but the tree it measures can
		// double at every level, so the sum saturates like the cost does:
		// 64 levels of two aliases wrapped it to -1, under any limit.
		m.complexity = costAdd(m.complexity, costAdd(1, sub.complexity))
		m.depth = max(m.depth, 1+sub.depth)
	}
	return m
}
