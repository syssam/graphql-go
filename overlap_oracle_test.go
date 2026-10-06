package graphql

// The merge rule as it was before it stopped flattening: every selection set
// collected into one list of fields, fragments included, and each list keyed
// by the fields in it. It is quadratic where one fragment is spread under
// many fields, which is why it was replaced, and it is kept here because it
// is short enough to read against the specification: the rule in overlap.go
// answers the same question by a longer route, and
// TestFieldsCanMergeAgreesWithTheFlatRule holds the two to one verdict.

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator/core"
)

var flatFieldsCanMergeRule = core.Rule{
	Name: "OverlappingFieldsCanBeMerged",
	RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		// One memo for the whole document: a fragment's fields are the same
		// AST nodes wherever it is spread, so a set checked under one
		// operation is not checked again under the next.
		m := &flatMergeCheck{
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

type flatMergeConflict struct {
	name   string
	reason string
	pos    *ast.Position
}

type flatMergeCheck struct {
	schema    *ast.Schema
	ids       map[*ast.Field]int
	subs      map[*ast.Field][]*ast.Field
	shape     map[string]bool
	common    map[string]bool
	conflicts []flatMergeConflict
	reported  map[string]bool
}

// collect returns the fields of set with inline fragments and fragment
// spreads expanded, each AST field once. A field its parent does not define
// stays in, untyped, because gqlparser still compares it by name.
func (m *flatMergeCheck) collect(set ast.SelectionSet) []*ast.Field {
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

func (m *flatMergeCheck) subFields(f *ast.Field) []*ast.Field {
	if s, ok := m.subs[f]; ok {
		return s
	}
	s := m.collect(f.SelectionSet)
	m.subs[f] = s
	return s
}

// merged is the union of the subselections of fields, each AST field once.
func (m *flatMergeCheck) merged(fields []*ast.Field) []*ast.Field {
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
func (m *flatMergeCheck) key(fields []*ast.Field) string {
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

func flatResponseName(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// groups splits fields by response name, in the order the names first appear.
func flatGroups(fields []*ast.Field) [][]*ast.Field {
	index := map[string]int{}
	var out [][]*ast.Field
	for _, f := range fields {
		n := flatResponseName(f)
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
func (m *flatMergeCheck) report(path []string, name string, pos *ast.Position, reason string) {
	full := flatUnder(path, name)
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
	m.conflicts = append(m.conflicts, flatMergeConflict{name: full[0], reason: b.String(), pos: pos})
}

// under is path extended by name, never sharing path's backing array: the
// sibling groups of one level each extend the same path.
func flatUnder(path []string, name string) []string {
	return append(slices.Clip(path), name)
}

// checkShape is SameResponseShape over every group in fields.
func (m *flatMergeCheck) checkShape(fields []*ast.Field, path []string) {
	k := m.key(fields)
	if m.shape[k] {
		return
	}
	m.shape[k] = true
	for _, g := range flatGroups(fields) {
		name := flatResponseName(g[0])
		if a, b, ok := m.shapeConflict(g); ok {
			m.report(path, name, b.Position, fmt.Sprintf(`they return conflicting types "%s" and "%s"`, a.Definition.Type.String(), b.Definition.Type.String()))
			continue
		}
		if sub := m.merged(g); len(sub) > 0 {
			m.checkShape(sub, flatUnder(path, name))
		}
	}
}

// shapeConflict finds two fields whose types doTypesConflict would refuse:
// wrapped differently in lists or non-null, or two different leaf types.
// A composite type conflicts with nothing at its own level, so the relation
// is not transitive, and comparing each field with the first would miss a
// leaf pair a composite field sits between.
func (m *flatMergeCheck) shapeConflict(g []*ast.Field) (a, b *ast.Field, ok bool) {
	var first *ast.Field
	var leaf *ast.Field
	for _, f := range g {
		if f.Definition == nil {
			continue
		}
		if first == nil {
			first = f
		} else if flatWrapping(first.Definition.Type) != flatWrapping(f.Definition.Type) {
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
func flatWrapping(t *ast.Type) string {
	var b strings.Builder
	for ; t != nil; t = t.Elem {
		if t.Elem != nil {
			b.WriteByte('[')
		}
		// Only the named type's own nullability: gqlparser's doTypesConflict
		// steps into a list without comparing the list's, and this rule has
		// to agree with it on what is valid, or the same selection would be
		// valid or not depending on how large the document around it is.
		if t.NonNull && t.Elem == nil {
			b.WriteByte('!')
		}
	}
	return b.String()
}

// checkCommon is SameForCommonParents over every group in fields. Two fields
// whose parents are different object types can never be selected on the
// same value, so only fields that could share a parent are compared: those
// on one object type together with those on an interface or union.
func (m *flatMergeCheck) checkCommon(fields []*ast.Field, path []string) {
	k := m.key(fields)
	if m.common[k] {
		return
	}
	m.common[k] = true
	for _, g := range flatGroups(fields) {
		name := flatResponseName(g[0])
		for _, part := range flatCommonParents(g) {
			if reason, at, ok := flatSameNameAndArgs(part); !ok {
				m.report(path, name, at.Position, reason)
				continue
			}
			if sub := m.merged(part); len(sub) > 0 {
				m.checkCommon(sub, flatUnder(path, name))
			}
		}
	}
}

func flatCommonParents(g []*ast.Field) [][]*ast.Field {
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
func flatSameNameAndArgs(part []*ast.Field) (string, *ast.Field, bool) {
	a := part[0]
	for _, b := range part[1:] {
		if a.Name != b.Name {
			return fmt.Sprintf(`"%s" and "%s" are different fields`, a.Name, b.Name), b, false
		}
		if !flatSameArguments(a.Arguments, b.Arguments) {
			return "they have differing arguments", b, false
		}
	}
	return "", nil, true
}

// sameArguments and sameValue are gqlparser's, which it does not export:
// object fields compare in name order, list elements in the order written.
func flatSameArguments(a, b ast.ArgumentList) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		matched := false
		for _, y := range b {
			if x.Name == y.Name && flatSameValue(x.Value, y.Value) {
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

func flatSameValue(a, b *ast.Value) bool {
	if a.Kind != b.Kind || a.Raw != b.Raw || len(a.Children) != len(b.Children) {
		return false
	}
	ca, cb := a.Children, b.Children
	if a.Kind == ast.ObjectValue {
		ca, cb = flatByName(ca), flatByName(cb)
	}
	for i := range ca {
		if ca[i].Name != cb[i].Name || !flatSameValue(ca[i].Value, cb[i].Value) {
			return false
		}
	}
	return true
}

func flatByName(c ast.ChildValueList) ast.ChildValueList {
	s := slices.Clone(c)
	slices.SortStableFunc(s, func(x, y *ast.ChildValue) int { return cmp.Compare(x.Name, y.Name) })
	return s
}
