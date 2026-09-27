package graphql

import "github.com/vektah/gqlparser/v2/ast"

// Default document limits, Apollo Router's: 15 000 tokens is gqlgen's
// default too, and far past what a client's largest real operation is
// measured at; nesting is kept well under the router's 500 recursion because
// here it bounds a validator cost that grows with its square.
const (
	defaultMaxTokens  = 15000
	defaultMaxNesting = 100
)

// fragmentLookupBudget is about 50 ms of gqlparser's fragment lookups (3 ns
// each, measured). An introspection query makes 38; a 500-fragment document
// shaped like a Relay application's, a tree ten deep, about a million; the
// fragment DAGs the limit exists for, 500 million and more. It has no option
// because no document a client writes comes near it, and it goes away when
// gqlparser's walker indexes its fragments.
const fragmentLookupBudget = 1 << 24

// documentShape is what one linear walk of a parsed document learns before
// the validator, whose default rules are not all linear in it, sees it.
type documentShape struct {
	// selections counts fields, fragment spreads and inline fragments as
	// written, each once however often a spread expands it.
	selections int
	// nesting is the deepest selection set or input value literal, in
	// levels as written: { a { b } } is 2, [[1]] is 2.
	nesting int
}

// measureDocument walks doc once, stopping its descent one level past limit
// so that a document refused for its depth is not also walked to the
// bottom. A limit of zero walks everything.
func measureDocument(doc *ast.QueryDocument, limit int) documentShape {
	m := documentMeasure{limit: limit}
	for _, op := range doc.Operations {
		for _, v := range op.VariableDefinitions {
			m.value(v.DefaultValue, 1)
			m.directives(v.Directives)
		}
		m.directives(op.Directives)
		m.selections(op.SelectionSet, 1)
	}
	for _, f := range doc.Fragments {
		m.directives(f.Directives)
		m.selections(f.SelectionSet, 1)
	}
	return m.shape
}

type documentMeasure struct {
	limit int
	shape documentShape
}

func (m *documentMeasure) deeper(depth int) bool {
	m.shape.nesting = max(m.shape.nesting, depth)
	return m.limit == 0 || depth <= m.limit
}

func (m *documentMeasure) selections(set ast.SelectionSet, depth int) {
	if len(set) == 0 || !m.deeper(depth) {
		return
	}
	for _, sel := range set {
		m.shape.selections++
		switch sel := sel.(type) {
		case *ast.Field:
			for _, a := range sel.Arguments {
				m.value(a.Value, 1)
			}
			m.directives(sel.Directives)
			m.selections(sel.SelectionSet, depth+1)
		case *ast.InlineFragment:
			m.directives(sel.Directives)
			m.selections(sel.SelectionSet, depth+1)
		case *ast.FragmentSpread:
			m.directives(sel.Directives)
		}
	}
}

func (m *documentMeasure) directives(ds ast.DirectiveList) {
	for _, d := range ds {
		for _, a := range d.Arguments {
			m.value(a.Value, 1)
		}
	}
}

func (m *documentMeasure) value(v *ast.Value, depth int) {
	if v == nil || len(v.Children) == 0 {
		return
	}
	if !m.deeper(depth) {
		return
	}
	for _, c := range v.Children {
		m.value(c.Value, depth+1)
	}
}

// fragmentLookups counts the fragment-list comparisons gqlparser's walker
// will make validating doc, stopping once the count passes budget. The
// walker walks each operation, then each fragment definition on its own,
// entering every fragment reachable from where it started once per walk, and
// finds each spread's definition by scanning doc.Fragments from the front. A
// fragment DAG -- each fragment spreading the next ones -- makes that cubic
// in the fragment count: 1 500 fragments in 15 000 tokens took 4.7 s.
func fragmentLookups(doc *ast.QueryDocument, budget int) int {
	n := len(doc.Fragments)
	if n == 0 {
		return 0
	}
	index := make(map[string]int, n)
	for i, f := range doc.Fragments {
		if _, dup := index[f.Name]; !dup {
			index[f.Name] = i
		}
	}
	// spreads[i] is fragment i's spreads, as indices; -1 for an unknown name.
	spreads := make([][]int, n)
	for i, f := range doc.Fragments {
		spreads[i] = spreadTargets(f.SelectionSet, index, nil)
	}
	stamp := make([]int, n)
	walks := 0
	total := 0
	walk := func(roots []int) bool {
		walks++
		// A copy: spreads[i] is appended to it, and roots may be one of them.
		stack := append([]int(nil), roots...)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			// A lookup scans to the definition, or the whole list for an
			// unknown name.
			if i < 0 {
				total += n
				continue
			}
			total += i + 1
			if total > budget {
				return false
			}
			if stamp[i] == walks {
				continue
			}
			stamp[i] = walks
			stack = append(stack, spreads[i]...)
		}
		return true
	}
	for _, op := range doc.Operations {
		if !walk(spreadTargets(op.SelectionSet, index, nil)) {
			return total
		}
	}
	for i := range doc.Fragments {
		if !walk(spreads[i]) {
			return total
		}
	}
	return total
}

func spreadTargets(set ast.SelectionSet, index map[string]int, out []int) []int {
	for _, sel := range set {
		switch sel := sel.(type) {
		case *ast.Field:
			out = spreadTargets(sel.SelectionSet, index, out)
		case *ast.InlineFragment:
			out = spreadTargets(sel.SelectionSet, index, out)
		case *ast.FragmentSpread:
			if i, ok := index[sel.Name]; ok {
				out = append(out, i)
			} else {
				out = append(out, -1)
			}
		}
	}
	return out
}
