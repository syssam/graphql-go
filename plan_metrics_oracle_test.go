package graphql

// The walks below computed depth and complexity from the compiled plan until
// operationMetrics replaced them with an equivalent walk over the document,
// which can run before the plan is built. They are kept as the oracle that
// pins the replacement: TestOperationMetricsMatchesPlan and
// FuzzOperationMetrics compare the two. Do not reintroduce them into the
// production path.

func planMetricsOracle(p *plan) planMetrics {
	return planMetrics{
		complexity: complexityOf(p.sel),
		depth:      depthOf(p.sel),
	}
}

func complexityOf(sel *selectionSet) int {
	return complexityMemo(sel, make(map[*selectionSet]int))
}

// complexityMemo carries the memo that makes the walk linear in the DAG rather
// than in the tree the DAG unfolds to.
func complexityMemo(sel *selectionSet, memo map[*selectionSet]int) int {
	if sel == nil {
		return 0
	}
	if n, ok := memo[sel]; ok {
		return n
	}
	count := func(fields []*planField) int {
		n := 0
		for _, f := range fields {
			n += 1 + complexityMemo(f.sub, memo)
		}
		return n
	}
	n := 0
	if sel.byType == nil {
		n = count(sel.fields)
	} else {
		for _, concrete := range sel.byType {
			n = max(n, count(concrete.fields))
		}
	}
	memo[sel] = n
	return n
}

func depthOf(sel *selectionSet) int {
	return depthMemo(sel, make(map[*selectionSet]int))
}

func depthMemo(sel *selectionSet, memo map[*selectionSet]int) int {
	if sel == nil {
		return 0
	}
	if n, ok := memo[sel]; ok {
		return n
	}
	walk := func(fields []*planField) int {
		d := 0
		for _, f := range fields {
			fd := 1
			if f.sub != nil {
				fd += depthMemo(f.sub, memo)
			}
			d = max(d, fd)
		}
		return d
	}
	d := 0
	if sel.byType == nil {
		d = walk(sel.fields)
	} else {
		for _, c := range sel.byType {
			d = max(d, walk(c.fields))
		}
	}
	memo[sel] = d
	return d
}
