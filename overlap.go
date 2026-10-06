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
// union of their subselections.
//
// What keeps it from being quadratic is that a union is never written out. A
// selection set is the fields it writes itself together with the fragments it
// spreads, each of those a set of its own that every spread of the fragment
// shares (fieldSet). Checking a union is checking each part once, whoever
// holds it, and then each pair of parts against each other by looking the
// smaller one's response names up in the larger. A fragment of thousands of
// fields under a thousand fields is then one large set checked once and a
// thousand lookups, where flattening every selection set into one list made
// it a thousand copies of the fragment.
//
// It treats types as gqlparser does (doTypesConflict), so the two agree on
// which documents are valid (TestFieldsCanMergeAgreesWithGqlparser). Its
// messages have gqlparser's wording, one per conflicting group.
var fieldsCanMergeRule = core.Rule{
	Name: "OverlappingFieldsCanBeMerged",
	RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		newMergeCheck().observe(observers, addError)
	},
}

// mergeRelation is one of the two halves of FieldsInSetCanMerge. They walk
// the same sets and remember what they have checked apart.
type mergeRelation uint8

const (
	sameShape mergeRelation = iota
	sameForCommonParents
	mergeRelations
)

// maxSetProbes is how many indexed sets a union may be made of before part of
// it is indexed as one. Two unions are checked against each other part by
// part, so it bounds the pairs that takes; a selection set that spreads a
// thousand small fragments would otherwise be half a million of them.
const maxSetProbes = 64

type mergeCheck struct {
	schema   *ast.Schema
	addError core.AddErrFunc
	reported map[string]bool

	nextSet   int
	subs      map[*ast.Field]*fieldSet
	unions    map[string]*fieldSet
	fragments map[*ast.FragmentDefinition]*fragmentSet
	stack     []*ast.FragmentDefinition
	nextFrag  int

	within  [mergeRelations]map[int]bool
	between [mergeRelations]map[[2]int]bool
	// limit is maxSetProbes, lowered by the tests that want every union to
	// take the paths a wide one does.
	limit int

	// steps counts the work done: fields indexed, parts gathered and names
	// looked up. Nothing reads it but the tests that hold the rule to growing
	// with the document rather than with its square.
	steps int
}

func newMergeCheck() *mergeCheck {
	m := &mergeCheck{
		reported:  map[string]bool{},
		subs:      map[*ast.Field]*fieldSet{},
		unions:    map[string]*fieldSet{},
		fragments: map[*ast.FragmentDefinition]*fragmentSet{},
		limit:     maxSetProbes,
	}
	for r := range m.within {
		m.within[r] = map[int]bool{}
		m.between[r] = map[[2]int]bool{}
	}
	return m
}

// observe checks every operation and every fragment. One mergeCheck serves
// the whole document: a fragment's fields are the same AST nodes wherever it
// is spread, so a set checked under one operation is not checked again under
// the next. A fragment is checked on its own as well, as gqlparser does: a
// document may be nothing but fragments.
func (m *mergeCheck) observe(observers *core.Events, addError core.AddErrFunc) {
	m.addError = addError
	check := func(root *fieldSet) {
		m.checkWithin(sameShape, root, nil)
		m.checkWithin(sameForCommonParents, root, nil)
	}
	observers.OnOperation(func(w *core.Walker, op *ast.OperationDefinition) {
		m.schema = w.Schema
		check(m.setOf(op.SelectionSet))
	})
	observers.OnFragment(func(w *core.Walker, f *ast.FragmentDefinition) {
		m.schema = w.Schema
		check(m.fragment(f))
	})
}

// fieldSet is a set of fields. A leaf holds the fields one selection set
// writes itself, inline fragments included; any other is the union of its
// kids. The same AST field is never in two leaves, and a set reached twice is
// one set, so identity is enough to tell that two sets have already met.
//
// A union answers for a response name by asking its kids, unless it is flat:
// then it has an index of its own over every field beneath it. Which unions
// are flat is settle's decision.
type fieldSet struct {
	id     int
	fields []*ast.Field
	kids   []*fieldSet
	// whole marks a fragment's set, which a union holding it keeps as one
	// kid. Any other union is dissolved into the union that takes it in, so
	// that what they share is recognised as shared.
	whole bool
	flat  bool
	// probes is how many indexed sets a lookup in this one asks.
	probes int
	index  *setIndex
	// count is how many fields a flat union's index holds, once counted.
	count int
}

func (s *fieldSet) indexed() bool { return s.fields != nil || s.flat }

type setIndex struct {
	groups []*fieldGroupSet
	byName map[string]*fieldGroupSet
}

// fieldGroupSet is the fields of one set that share a response name, with
// what the two relations need of them worked out once.
type fieldGroupSet struct {
	name   string
	fields []*ast.Field

	shaped bool
	shape  groupShape

	parted bool
	common groupParents

	subAll     *fieldSet
	subAllDone bool
}

// alike is a list of fields reduced to what comparing it with another list
// needs: one of them, and the first that differs from that one, if any does.
// When the property compared is an equivalence, every field of one list
// agrees with every field of another exactly when neither has an odd one and
// the two firsts agree.
type alike struct {
	first, odd *ast.Field
}

// across finds a field of x and a field of y that differ, given that same
// reports whether two fields agree.
func across(x, y alike, same func(a, b *ast.Field) bool) (a, b *ast.Field, differ bool) {
	switch {
	case x.first == nil || y.first == nil:
		return nil, nil, false
	case !same(x.first, y.first):
		return x.first, y.first, true
	case x.odd != nil:
		return x.odd, y.first, true
	case y.odd != nil:
		return x.first, y.odd, true
	}
	return nil, nil, false
}

// groupShape is what SameResponseShape needs of a group: its fields that have
// a type, alike in how that type is wrapped, and those of a leaf type, alike
// in which.
type groupShape struct {
	typed, leaves alike
}

// groupParents is a group split the way SameForCommonParents compares it:
// the fields on each object type, and those on an interface or union, or
// untyped, which could share a parent with any of them. Each list is kept
// alike in name and arguments, and so is the whole group.
type groupParents struct {
	order    []string
	byParent map[string][]*ast.Field
	abstract []*ast.Field

	all       alike
	abstracts alike
	parents   map[string]alike

	subAbstract *fieldSet
	subParent   map[string]*fieldSet
	subPart     map[string]*fieldSet
	subDone     bool
}

func (m *mergeCheck) leaf(fields []*ast.Field) *fieldSet {
	if len(fields) == 0 {
		return nil
	}
	m.nextSet++
	return &fieldSet{id: m.nextSet, fields: fields, probes: 1}
}

// direct splits a selection set into the fields it writes and the fragments
// it spreads, looking through inline fragments. A field its parent does not
// define stays in, untyped, because gqlparser still compares it by name.
func (m *mergeCheck) direct(set ast.SelectionSet) ([]*ast.Field, []*ast.FragmentDefinition) {
	var fields []*ast.Field
	var spreads []*ast.FragmentDefinition
	var seen map[*ast.FragmentDefinition]bool
	var walk func(ast.SelectionSet)
	walk = func(set ast.SelectionSet) {
		for _, sel := range set {
			m.steps++
			switch sel := sel.(type) {
			case *ast.Field:
				if sel.ObjectDefinition != nil {
					fields = append(fields, sel)
				}
			case *ast.InlineFragment:
				walk(sel.SelectionSet)
			case *ast.FragmentSpread:
				if sel.Definition == nil || seen[sel.Definition] {
					continue
				}
				if seen == nil {
					seen = map[*ast.FragmentDefinition]bool{}
				}
				seen[sel.Definition] = true
				spreads = append(spreads, sel.Definition)
			}
		}
	}
	walk(set)
	return fields, spreads
}

// setOf is the set a selection set collects: its own fields and every
// fragment it spreads.
func (m *mergeCheck) setOf(set ast.SelectionSet) *fieldSet {
	fields, spreads := m.direct(set)
	parts := make([]*fieldSet, 0, 1+len(spreads))
	parts = append(parts, m.leaf(fields))
	for _, def := range spreads {
		parts = append(parts, m.fragment(def))
	}
	return m.union(parts)
}

func (m *mergeCheck) sub(f *ast.Field) *fieldSet {
	if s, ok := m.subs[f]; ok {
		return s
	}
	s := m.setOf(f.SelectionSet)
	m.subs[f] = s
	return s
}

// fragmentSet is a fragment's place in the walk that builds fragment sets,
// which is Tarjan's: fragments that spread one another in a cycle collect the
// same fields, and get the one set. Such a document is refused by another
// rule, but this one still runs on it.
type fragmentSet struct {
	index, low int
	onStack    bool
	fields     *fieldSet
	spreads    []*ast.FragmentDefinition
	set        *fieldSet
}

// fragment is the set of a fragment: the fields it writes and the sets of the
// fragments it spreads, shared by every spread of it.
func (m *mergeCheck) fragment(def *ast.FragmentDefinition) *fieldSet {
	st := m.fragments[def]
	if st == nil {
		st = m.connect(def)
	}
	return st.set
}

func (m *mergeCheck) connect(def *ast.FragmentDefinition) *fragmentSet {
	st := &fragmentSet{index: m.nextFrag, low: m.nextFrag, onStack: true}
	m.nextFrag++
	m.fragments[def] = st
	m.stack = append(m.stack, def)
	fields, spreads := m.direct(def.SelectionSet)
	st.fields, st.spreads = m.leaf(fields), spreads
	for _, to := range spreads {
		switch ts := m.fragments[to]; {
		case ts == nil:
			st.low = min(st.low, m.connect(to).low)
		case ts.onStack:
			st.low = min(st.low, ts.index)
		}
	}
	if st.low != st.index {
		return st
	}
	// def closes a component: everything above it on the stack spreads its
	// way back to it. Whatever else they spread was closed earlier and has
	// its set.
	at := len(m.stack) - 1
	for m.stack[at] != def {
		at--
	}
	component := m.stack[at:]
	m.stack = m.stack[:at]
	var parts []*fieldSet
	for _, member := range component {
		m.fragments[member].onStack = false
	}
	for _, member := range component {
		ms := m.fragments[member]
		parts = append(parts, ms.fields)
		for _, to := range ms.spreads {
			if ts := m.fragments[to]; !ts.onStack && ts.set != nil {
				parts = append(parts, ts.set)
			}
		}
	}
	set := m.join(parts, true)
	for _, member := range component {
		m.fragments[member].set = set
	}
	return st
}

// union is the set holding everything in parts. A union that is not a
// fragment's is dissolved into it, so the result is made of leaves, fragment
// sets and flat sets, each once.
func (m *mergeCheck) union(parts []*fieldSet) *fieldSet {
	return m.join(parts, false)
}

func (m *mergeCheck) join(parts []*fieldSet, whole bool) *fieldSet {
	kids := make([]*fieldSet, 0, len(parts))
	for _, p := range parts {
		switch {
		case p == nil:
		case p.indexed() || p.whole:
			kids = append(kids, p)
		default:
			kids = append(kids, p.kids...)
		}
	}
	m.steps += len(kids)
	slices.SortFunc(kids, func(a, b *fieldSet) int { return cmp.Compare(a.id, b.id) })
	kids = slices.CompactFunc(kids, func(a, b *fieldSet) bool { return a == b })
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return kids[0]
	}
	var key string
	if !whole {
		var b []byte
		for _, k := range kids {
			b = strconv.AppendInt(b, int64(k.id), 36)
			b = append(b, ',')
		}
		key = string(b)
		if u, ok := m.unions[key]; ok {
			return u
		}
	}
	m.nextSet++
	u := &fieldSet{id: m.nextSet, kids: kids, whole: whole}
	m.settle(u)
	if !whole {
		m.unions[key] = u
	}
	return u
}

// settle keeps a union within the probe limit by giving part of it an index
// of its own. Which part is a question of what is shared. A kid that is
// itself a union is a fragment's set, the same one in every union that
// spreads the fragment, so an index on it is paid for once and serves them
// all, where an index on this union is paid for again by the next one like
// it. That is right unless those kids are mostly the same fields as each
// other -- a thousand small fragments that each spread one large one -- when
// an index apiece would be the large one a thousand times over, and they are
// indexed together instead, as one set that any union of the same fragments
// shares. Only a union of more single sets than the limit is indexed itself.
func (m *mergeCheck) settle(u *fieldSet) {
	total := 0
	var wide []*fieldSet
	for _, k := range u.kids {
		total += k.probes
		if k.probes > 1 {
			wide = append(wide, k)
		}
	}
	if total > m.limit && len(wide) > 0 {
		apart := 0
		for _, k := range wide {
			apart += m.fieldsUnder(k)
		}
		if len(wide) == 1 || apart <= 2*m.fieldsUnder(wide...) {
			for _, k := range wide {
				total -= k.probes - 1
				k.flat, k.probes = true, 1
			}
		} else {
			kept := u.kids[:0:0]
			for _, k := range u.kids {
				if k.probes == 1 {
					kept = append(kept, k)
				}
			}
			kept = append(kept, m.together(wide))
			u.kids = kept
			total = len(u.kids)
		}
	}
	if total > m.limit {
		u.flat, total = true, 1
	}
	u.probes = total
}

// together is the flat union of sets, one for all that ask for the same.
func (m *mergeCheck) together(sets []*fieldSet) *fieldSet {
	b := []byte("flat:")
	for _, k := range sets {
		b = strconv.AppendInt(b, int64(k.id), 36)
		b = append(b, ',')
	}
	key := string(b)
	if u, ok := m.unions[key]; ok {
		return u
	}
	m.nextSet++
	u := &fieldSet{id: m.nextSet, kids: sets, flat: true, probes: 1}
	m.unions[key] = u
	return u
}

// fieldsUnder counts the fields beneath sets, taking each indexed set among
// them once. It is what an index over them all would hold, near enough for
// settle: a flat set and a leaf beneath it are both counted.
func (m *mergeCheck) fieldsUnder(sets ...*fieldSet) int {
	n := 0
	seen := map[*fieldSet]bool{}
	var walk func(*fieldSet)
	walk = func(s *fieldSet) {
		if seen[s] {
			return
		}
		seen[s] = true
		m.steps++
		if s.indexed() {
			n += m.fieldsIn(s)
			return
		}
		for _, k := range s.kids {
			walk(k)
		}
	}
	for _, s := range sets {
		walk(s)
	}
	return n
}

// fieldsIn counts the fields of an indexed set.
func (m *mergeCheck) fieldsIn(s *fieldSet) int {
	if s.fields != nil {
		return len(s.fields)
	}
	if s.count == 0 {
		for _, l := range m.leaves(s) {
			s.count += len(l.fields)
		}
	}
	return s.count
}

// indexOf is the index of an indexed set: of its own fields for a leaf, and
// for a flat union of every field beneath it.
func (m *mergeCheck) indexOf(s *fieldSet) *setIndex {
	if s.index != nil {
		return s.index
	}
	ix := &setIndex{byName: map[string]*fieldGroupSet{}}
	add := func(fields []*ast.Field) {
		m.steps += len(fields)
		for _, f := range fields {
			name := responseName(f)
			g := ix.byName[name]
			if g == nil {
				g = &fieldGroupSet{name: name}
				ix.byName[name] = g
				ix.groups = append(ix.groups, g)
			}
			g.fields = append(g.fields, f)
		}
	}
	if s.fields != nil {
		add(s.fields)
	} else {
		// Leaves are numbered as the document is read, so this is the
		// order a reader meets the fields in, whatever order the kids
		// were joined in.
		for _, l := range m.leaves(s) {
			add(l.fields)
		}
	}
	s.index = ix
	return ix
}

// leaves are the leaves beneath s, each once.
func (m *mergeCheck) leaves(s *fieldSet) []*fieldSet {
	var out []*fieldSet
	seen := map[*fieldSet]bool{}
	var walk func(*fieldSet)
	walk = func(s *fieldSet) {
		if seen[s] {
			return
		}
		seen[s] = true
		m.steps++
		if s.fields != nil {
			out = append(out, s)
			return
		}
		for _, k := range s.kids {
			walk(k)
		}
	}
	walk(s)
	slices.SortFunc(out, func(a, b *fieldSet) int { return cmp.Compare(a.id, b.id) })
	return out
}

func responseName(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
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
	m.addError(core.Message(`Fields "%s" conflict because %s. Use different aliases on the fields to fetch both if this was intentional.`, full[0], b.String()), core.At(pos))
}

// under is path extended by name, never sharing path's backing array: the
// sibling groups of one level each extend the same path.
func under(path []string, name string) []string {
	return append(slices.Clip(path), name)
}

// checkWithin holds the relation over every pair of fields in s. A set is
// checked once however many selection sets hold it; it is marked before it is
// walked, which is also what ends the walk of a fragment that reaches itself
// through a field.
func (m *mergeCheck) checkWithin(rel mergeRelation, s *fieldSet, path []string) {
	if s == nil || m.within[rel][s.id] {
		return
	}
	m.within[rel][s.id] = true
	if !s.indexed() {
		for _, k := range s.kids {
			m.checkWithin(rel, k, path)
		}
		for i, a := range s.kids {
			for _, b := range s.kids[i+1:] {
				m.checkBetween(rel, a, b, path)
			}
		}
		return
	}
	for _, g := range m.indexOf(s).groups {
		if rel == sameShape {
			m.shapeWithin(g, path)
		} else {
			m.commonWithin(g, path)
		}
	}
}

// checkBetween holds the relation over every pair made of one field from a
// and one from b. Each set is checked within itself wherever it is held, so
// the same set on both sides has nothing left to compare. A union is taken
// apart down to the sets that have an index, and those are compared by
// looking each response name of the smaller up in the larger.
func (m *mergeCheck) checkBetween(rel mergeRelation, a, b *fieldSet, path []string) {
	if a == nil || b == nil || a == b {
		return
	}
	pair := [2]int{min(a.id, b.id), max(a.id, b.id)}
	if m.between[rel][pair] {
		return
	}
	m.between[rel][pair] = true
	m.steps++
	switch {
	case !a.indexed():
		for _, k := range a.kids {
			m.checkBetween(rel, k, b, path)
		}
		return
	case !b.indexed():
		for _, k := range b.kids {
			m.checkBetween(rel, a, k, path)
		}
		return
	}
	small, large := m.indexOf(a), m.indexOf(b)
	if len(large.groups) < len(small.groups) {
		small, large = large, small
	}
	m.steps += len(small.groups)
	for _, x := range small.groups {
		y := large.byName[x.name]
		if y == nil {
			continue
		}
		if rel == sameShape {
			m.shapeBetween(x, y, path)
		} else {
			m.commonBetween(x, y, path)
		}
	}
}

// subAllOf is the union of the subselections of every field in g.
func (m *mergeCheck) subAllOf(g *fieldGroupSet) *fieldSet {
	if !g.subAllDone {
		g.subAll, g.subAllDone = m.subsOf(g.fields), true
	}
	return g.subAll
}

func (m *mergeCheck) subsOf(fields []*ast.Field) *fieldSet {
	if len(fields) == 1 {
		return m.sub(fields[0])
	}
	parts := make([]*fieldSet, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, m.sub(f))
	}
	return m.union(parts)
}

func (m *mergeCheck) shapeOf(g *fieldGroupSet) *groupShape {
	if g.shaped {
		return &g.shape
	}
	g.shaped = true
	m.steps += len(g.fields)
	sh := &g.shape
	for _, f := range g.fields {
		if f.Definition == nil {
			continue
		}
		switch {
		case sh.typed.first == nil:
			sh.typed.first = f
		case sh.typed.odd == nil && !sameWrapping(sh.typed.first, f):
			sh.typed.odd = f
		}
		if !m.isLeaf(f) {
			continue
		}
		switch {
		case sh.leaves.first == nil:
			sh.leaves.first = f
		case sh.leaves.odd == nil && !sameNamedType(sh.leaves.first, f):
			sh.leaves.odd = f
		}
	}
	return sh
}

func (m *mergeCheck) isLeaf(f *ast.Field) bool {
	t := m.schema.Types[f.Definition.Type.Name()]
	return t != nil && (t.Kind == ast.Scalar || t.Kind == ast.Enum)
}

func sameWrapping(a, b *ast.Field) bool {
	return wrapping(a.Definition.Type) == wrapping(b.Definition.Type)
}

func sameNamedType(a, b *ast.Field) bool {
	return a.Definition.Type.Name() == b.Definition.Type.Name()
}

func (m *mergeCheck) reportTypes(path []string, name string, a, b *ast.Field) {
	a, b = inOrder(a, b)
	m.report(path, name, b.Position, fmt.Sprintf(`they return conflicting types "%s" and "%s"`, a.Definition.Type.String(), b.Definition.Type.String()))
}

// shapeWithin is SameResponseShape for one group of one set.
func (m *mergeCheck) shapeWithin(g *fieldGroupSet, path []string) {
	if a, b, ok := m.shapeConflict(g.fields); ok {
		m.reportTypes(path, g.name, a, b)
		return
	}
	m.checkWithin(sameShape, m.subAllOf(g), under(path, g.name))
}

// shapeBetween is SameResponseShape between the groups two sets have under
// one response name: every typed field of one wrapped as every typed field of
// the other, and every leaf of one the type of every leaf of the other. A
// composite type conflicts with nothing at its own level, which is why the
// leaves are a list of their own rather than compared through the first typed
// field.
func (m *mergeCheck) shapeBetween(x, y *fieldGroupSet, path []string) {
	sx, sy := m.shapeOf(x), m.shapeOf(y)
	if a, b, differ := across(sx.typed, sy.typed, sameWrapping); differ {
		m.reportTypes(path, x.name, a, b)
		return
	}
	if a, b, differ := across(sx.leaves, sy.leaves, sameNamedType); differ {
		m.reportTypes(path, x.name, a, b)
		return
	}
	m.checkBetween(sameShape, m.subAllOf(x), m.subAllOf(y), under(path, x.name))
}

// inOrder puts the field written first first, so that a conflict between two
// sets is worded and placed as one inside a set is.
func inOrder(a, b *ast.Field) (*ast.Field, *ast.Field) {
	if a.Position != nil && b.Position != nil && b.Position.Start < a.Position.Start {
		return b, a
	}
	return a, b
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
		if m.isLeaf(f) {
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

// parentsOf splits g for SameForCommonParents. Two fields whose parents are
// different object types can never be selected on the same value, so only
// fields that could share a parent are compared: those on one object type
// together with those on an interface or union. gqlparser holds two fields
// apart only when both are typed and on different object types, so an untyped
// one is compared with all.
func (m *mergeCheck) parentsOf(g *fieldGroupSet) *groupParents {
	if g.parted {
		return &g.common
	}
	g.parted = true
	m.steps += len(g.fields)
	p := &g.common
	for _, f := range g.fields {
		if f.ObjectDefinition.Kind != ast.Object || f.Definition == nil {
			p.abstract = append(p.abstract, f)
			continue
		}
		name := f.ObjectDefinition.Name
		if p.byParent == nil {
			p.byParent = map[string][]*ast.Field{}
		}
		if _, ok := p.byParent[name]; !ok {
			p.order = append(p.order, name)
		}
		p.byParent[name] = append(p.byParent[name], f)
	}
	p.all = alikeInNameAndArgs(g.fields)
	p.abstracts = alikeInNameAndArgs(p.abstract)
	p.parents = make(map[string]alike, len(p.order))
	for _, name := range p.order {
		p.parents[name] = alikeInNameAndArgs(p.byParent[name])
	}
	return p
}

func alikeInNameAndArgs(fields []*ast.Field) alike {
	var l alike
	for i, f := range fields {
		if i == 0 {
			l.first = f
		} else if !sameNameAndArguments(l.first, f) {
			l.odd = f
			break
		}
	}
	return l
}

// within finds two fields of one list that differ.
func (l alike) within() (a, b *ast.Field, differ bool) {
	return l.first, l.odd, l.odd != nil
}

// subsets works out, once, the unions of subselections the group's parts
// need: of the abstract fields, of each object type's fields, and of each
// part, which is the two together.
func (m *mergeCheck) subsets(p *groupParents) {
	if p.subDone {
		return
	}
	p.subDone = true
	if len(p.abstract) > 0 {
		p.subAbstract = m.subsOf(p.abstract)
	}
	p.subParent = make(map[string]*fieldSet, len(p.order))
	p.subPart = make(map[string]*fieldSet, len(p.order))
	for _, name := range p.order {
		own := m.subsOf(p.byParent[name])
		p.subParent[name] = own
		p.subPart[name] = m.union([]*fieldSet{own, p.subAbstract})
	}
}

func (m *mergeCheck) reportFields(path []string, name string, a, b *ast.Field) {
	a, b = inOrder(a, b)
	reason := "they have differing arguments"
	if a.Name != b.Name {
		reason = fmt.Sprintf(`"%s" and "%s" are different fields`, a.Name, b.Name)
	}
	m.report(path, name, b.Position, reason)
}

// commonWithin is SameForCommonParents for one group of one set. The fields
// that must agree are each object type's together with the abstract ones, or
// the abstract ones alone when no field is on an object type, and the
// subselections of each such part are held to the same in turn.
func (m *mergeCheck) commonWithin(g *fieldGroupSet, path []string) {
	p := m.parentsOf(g)
	m.subsets(p)
	below := under(path, g.name)
	if len(p.order) == 0 {
		if a, b, differ := p.abstracts.within(); differ {
			m.reportFields(path, g.name, a, b)
			return
		}
		m.checkWithin(sameForCommonParents, p.subAbstract, below)
		return
	}
	m.steps += len(p.order)
	for _, name := range p.order {
		own := p.parents[name]
		a, b, differ := own.within()
		if !differ {
			a, b, differ = across(own, p.abstracts, sameNameAndArguments)
		}
		if differ {
			m.reportFields(path, g.name, a, b)
			continue
		}
		m.checkWithin(sameForCommonParents, p.subPart[name], below)
	}
}

// commonBetween is SameForCommonParents between the groups two sets have
// under one response name. The fields of one side that could share a parent
// with a field of the other are its abstract ones, with any, and those on an
// object type, with the same type's.
func (m *mergeCheck) commonBetween(x, y *fieldGroupSet, path []string) {
	px, py := m.parentsOf(x), m.parentsOf(y)
	a, b, differ := across(px.abstracts, py.all, sameNameAndArguments)
	if !differ {
		a, b, differ = across(px.all, py.abstracts, sameNameAndArguments)
	}
	small, large := px, py
	if len(large.order) < len(small.order) {
		small, large = large, small
	}
	m.steps += len(small.order)
	for i := 0; !differ && i < len(small.order); i++ {
		a, b, differ = across(small.parents[small.order[i]], large.parents[small.order[i]], sameNameAndArguments)
	}
	if differ {
		m.reportFields(path, x.name, a, b)
		return
	}

	m.subsets(px)
	m.subsets(py)
	below := under(path, x.name)
	m.checkBetween(sameForCommonParents, px.subAbstract, m.subAllOf(y), below)
	m.checkBetween(sameForCommonParents, m.subAllOf(x), py.subAbstract, below)
	for _, name := range small.order {
		if other := large.subParent[name]; other != nil {
			m.checkBetween(sameForCommonParents, small.subParent[name], other, below)
		}
	}
}

func sameNameAndArguments(a, b *ast.Field) bool {
	return a.Name == b.Name && sameArguments(a.Arguments, b.Arguments)
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
