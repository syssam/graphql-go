package graphql

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator/core"
)

// overlapExactMaxSelections is the largest document, counted in field
// selections, fragment spreads and inline fragments, that gqlparser's own
// OverlappingFieldsCanBeMerged checks. Its algorithm is graphql-js's: it
// compares every pair of fields sharing a response name, and re-collects a
// selection set's fields at every level above it, so a 10 KB document of one
// repeated field took 1.2 s and 1.25 GB, and 43 KB of aliases with differing
// arguments 2 s and 1.2 GB building n^2/2 errors. Below this size it keeps
// its exact messages; above it fieldsCanMergeRule decides.
const overlapExactMaxSelections = 256

// fieldsCanMergeRule is the specification's FieldsInSetCanMerge, checked the
// way Sangria and graphql-java check it rather than pair by pair: the fields
// sharing a response name are one group, SameResponseShape holds for a group
// when their types agree and it holds again for the union of their
// subselections, and SameForCommonParents holds when every field that could
// share a parent with another has its name and arguments, and again for the
// union of their subselections. Each union is memoized on the set of fields
// it holds, so a fragment spread under many fields is checked once, and the
// work is linear in the document rather than quadratic in each group.
//
// It treats types as gqlparser does (doTypesConflict), so the two agree on
// which documents are valid (TestFieldsCanMergeAgreesWithGqlparser). Its
// messages have gqlparser's wording, one per conflicting group.
var fieldsCanMergeRule = core.Rule{
	Name: "OverlappingFieldsCanBeMerged",
	RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		// One memo for the whole document: a fragment's fields are the same
		// AST nodes wherever it is spread, so a set checked under one
		// operation is not checked again under the next.
		m := &mergeCheck{
			ids:      map[*ast.Field]int{},
			subs:     map[*ast.Field][]*ast.Field{},
			shape:    map[string]bool{},
			common:   map[string]bool{},
			reported: map[string]bool{},
		}
		check := func(w *core.Walker, set ast.SelectionSet) {
			m.schema = w.Schema
			fields := m.collect(set)
			m.checkShape(fields, nil)
			m.checkCommon(fields, nil)
			for _, c := range m.conflicts {
				addError(core.Message(`Fields "%s" conflict because %s. Use different aliases on the fields to fetch both if this was intentional.`, c.name, c.reason), core.At(c.pos))
			}
			m.conflicts = m.conflicts[:0]
		}
		// A fragment is checked on its own as well, as gqlparser does: a
		// document may be nothing but fragments.
		observers.OnOperation(func(w *core.Walker, op *ast.OperationDefinition) { check(w, op.SelectionSet) })
		observers.OnFragment(func(w *core.Walker, f *ast.FragmentDefinition) { check(w, f.SelectionSet) })
	},
}

type mergeConflict struct {
	name   string
	reason string
	pos    *ast.Position
}

type mergeCheck struct {
	schema    *ast.Schema
	ids       map[*ast.Field]int
	subs      map[*ast.Field][]*ast.Field
	shape     map[string]bool
	common    map[string]bool
	conflicts []mergeConflict
	reported  map[string]bool
}

// collect returns the fields of set with inline fragments and fragment
// spreads expanded, each AST field once. A field its parent does not define
// stays in, untyped, because gqlparser still compares it by name.
func (m *mergeCheck) collect(set ast.SelectionSet) []*ast.Field {
	var out []*ast.Field
	seen := map[*ast.Field]bool{}
	frags := map[string]bool{}
	var walk func(ast.SelectionSet)
	walk = func(set ast.SelectionSet) {
		for _, sel := range set {
			switch sel := sel.(type) {
			case *ast.Field:
				if sel.ObjectDefinition == nil || seen[sel] {
					continue
				}
				seen[sel] = true
				if _, ok := m.ids[sel]; !ok {
					m.ids[sel] = len(m.ids)
				}
				out = append(out, sel)
			case *ast.InlineFragment:
				walk(sel.SelectionSet)
			case *ast.FragmentSpread:
				if sel.Definition == nil || frags[sel.Name] {
					continue
				}
				frags[sel.Name] = true
				walk(sel.Definition.SelectionSet)
			}
		}
	}
	walk(set)
	return out
}

func (m *mergeCheck) subFields(f *ast.Field) []*ast.Field {
	if s, ok := m.subs[f]; ok {
		return s
	}
	s := m.collect(f.SelectionSet)
	m.subs[f] = s
	return s
}

// merged is the union of the subselections of fields, each AST field once.
func (m *mergeCheck) merged(fields []*ast.Field) []*ast.Field {
	if len(fields) == 1 {
		return m.subFields(fields[0])
	}
	var out []*ast.Field
	seen := map[*ast.Field]bool{}
	for _, f := range fields {
		for _, s := range m.subFields(f) {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// key identifies a set of fields independently of the order it was
// collected in, for the memo.
func (m *mergeCheck) key(fields []*ast.Field) string {
	ids := make([]int, len(fields))
	for i, f := range fields {
		ids[i] = m.ids[f]
	}
	slices.Sort(ids)
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(strconv.Itoa(id))
		b.WriteByte(',')
	}
	return b.String()
}

func responseName(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// groups splits fields by response name, in the order the names first appear.
func groups(fields []*ast.Field) [][]*ast.Field {
	index := map[string]int{}
	var out [][]*ast.Field
	for _, f := range fields {
		n := responseName(f)
		i, ok := index[n]
		if !ok {
			i = len(out)
			index[n] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], f)
	}
	return out
}

// report records one conflict per response path, worded as gqlparser words
// a nested one: Fields "a" conflict because subfields "b" conflict because ...
func (m *mergeCheck) report(path []string, name string, pos *ast.Position, reason string) {
	full := under(path, name)
	var b strings.Builder
	for _, sub := range full[1:] {
		fmt.Fprintf(&b, `subfields "%s" conflict because `, sub)
	}
	b.WriteString(reason)
	// A conflict inside a fragment is found from every operation spreading
	// it and from the fragment itself; it is one conflict.
	k := fmt.Sprintf("%s\x00%s\x00%v", full[0], b.String(), pos)
	if m.reported[k] {
		return
	}
	m.reported[k] = true
	m.conflicts = append(m.conflicts, mergeConflict{name: full[0], reason: b.String(), pos: pos})
}

// under is path extended by name, never sharing path's backing array: the
// sibling groups of one level each extend the same path.
func under(path []string, name string) []string {
	return append(slices.Clip(path), name)
}

// checkShape is SameResponseShape over every group in fields.
func (m *mergeCheck) checkShape(fields []*ast.Field, path []string) {
	k := m.key(fields)
	if m.shape[k] {
		return
	}
	m.shape[k] = true
	for _, g := range groups(fields) {
		name := responseName(g[0])
		if a, b, ok := m.shapeConflict(g); ok {
			m.report(path, name, b.Position, fmt.Sprintf(`they return conflicting types "%s" and "%s"`, a.Definition.Type.String(), b.Definition.Type.String()))
			continue
		}
		if sub := m.merged(g); len(sub) > 0 {
			m.checkShape(sub, under(path, name))
		}
	}
}

// shapeConflict finds two fields whose types doTypesConflict would refuse:
// wrapped differently in lists or non-null, or two different leaf types.
// A composite type conflicts with nothing at its own level, so the relation
// is not transitive, and comparing each field with the first would miss a
// leaf pair a composite field sits between.
func (m *mergeCheck) shapeConflict(g []*ast.Field) (a, b *ast.Field, ok bool) {
	var first *ast.Field
	var leaf *ast.Field
	for _, f := range g {
		if f.Definition == nil {
			continue
		}
		if first == nil {
			first = f
		} else if wrapping(first.Definition.Type) != wrapping(f.Definition.Type) {
			return first, f, true
		}
		if t := m.schema.Types[f.Definition.Type.Name()]; t != nil && (t.Kind == ast.Scalar || t.Kind == ast.Enum) {
			if leaf == nil {
				leaf = f
			} else if leaf.Definition.Type.Name() != f.Definition.Type.Name() {
				return leaf, f, true
			}
		}
	}
	return nil, nil, false
}

// wrapping is t's list and non-null structure without the named type.
func wrapping(t *ast.Type) string {
	var b strings.Builder
	for ; t != nil; t = t.Elem {
		if t.Elem != nil {
			b.WriteByte('[')
		}
		if t.NonNull {
			b.WriteByte('!')
		}
	}
	return b.String()
}

// checkCommon is SameForCommonParents over every group in fields. Two fields
// whose parents are different object types can never be selected on the
// same value, so only fields that could share a parent are compared: those
// on one object type together with those on an interface or union.
func (m *mergeCheck) checkCommon(fields []*ast.Field, path []string) {
	k := m.key(fields)
	if m.common[k] {
		return
	}
	m.common[k] = true
	for _, g := range groups(fields) {
		name := responseName(g[0])
		for _, part := range commonParents(g) {
			if reason, at, ok := sameNameAndArgs(part); !ok {
				m.report(path, name, at.Position, reason)
				continue
			}
			if sub := m.merged(part); len(sub) > 0 {
				m.checkCommon(sub, under(path, name))
			}
		}
	}
}

func commonParents(g []*ast.Field) [][]*ast.Field {
	var abstract []*ast.Field
	var order []string
	byParent := map[string][]*ast.Field{}
	for _, f := range g {
		// gqlparser holds two fields apart only when both are typed and on
		// different object types, so an untyped one is compared with all.
		if f.ObjectDefinition.Kind != ast.Object || f.Definition == nil {
			abstract = append(abstract, f)
			continue
		}
		p := f.ObjectDefinition.Name
		if _, ok := byParent[p]; !ok {
			order = append(order, p)
		}
		byParent[p] = append(byParent[p], f)
	}
	if len(order) == 0 {
		return [][]*ast.Field{abstract}
	}
	out := make([][]*ast.Field, 0, len(order))
	for _, p := range order {
		out = append(out, append(byParent[p], abstract...))
	}
	return out
}

// sameNameAndArgs compares each field with the first, which is enough:
// equal names and equal argument sets are both equivalence relations.
func sameNameAndArgs(part []*ast.Field) (string, *ast.Field, bool) {
	a := part[0]
	for _, b := range part[1:] {
		if a.Name != b.Name {
			return fmt.Sprintf(`"%s" and "%s" are different fields`, a.Name, b.Name), b, false
		}
		if !sameArguments(a.Arguments, b.Arguments) {
			return "they have differing arguments", b, false
		}
	}
	return "", nil, true
}

// sameArguments and sameValue are gqlparser's, which it does not export:
// object fields compare in name order, list elements in the order written.
func sameArguments(a, b ast.ArgumentList) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		matched := false
		for _, y := range b {
			if x.Name == y.Name && sameValue(x.Value, y.Value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func sameValue(a, b *ast.Value) bool {
	if a.Kind != b.Kind || a.Raw != b.Raw || len(a.Children) != len(b.Children) {
		return false
	}
	ca, cb := a.Children, b.Children
	if a.Kind == ast.ObjectValue {
		ca, cb = byName(ca), byName(cb)
	}
	for i := range ca {
		if ca[i].Name != cb[i].Name || !sameValue(ca[i].Value, cb[i].Value) {
			return false
		}
	}
	return true
}

func byName(c ast.ChildValueList) ast.ChildValueList {
	s := slices.Clone(c)
	slices.SortStableFunc(s, func(x, y *ast.ChildValue) int { return cmp.Compare(x.Name, y.Name) })
	return s
}
