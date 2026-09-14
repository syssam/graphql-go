package graphql

import (
	"container/list"
	"context"
	"hash/maphash"
	"slices"
	"sort"
	"sync"

	"github.com/syssam/graphql-go/internal/jsonw"
	"github.com/vektah/gqlparser/v2/ast"
)

// maxCondVars bounds the number of Boolean variables referenced by @skip and
// @include for which plan variants are cached.
const maxCondVars = 16

// docEntry is a parsed and validated document together with the plans
// compiled from it, one per (operation, skip/include variant).
type docEntry struct {
	query    string
	doc      *ast.QueryDocument
	condVars []string

	mu    sync.Mutex
	plans map[planKey]*plan
}

type planKey struct {
	op      string
	variant uint16
}

// plan is an immutable, fully resolved execution plan for one operation.
type plan struct {
	op         *ast.OperationDefinition
	root       *objectType
	sel        *selectionSet
	complexity int
	depth      int
}

// selectionSet is the flattened selection for a composite value. Concrete
// parents use fields; abstract parents carry one concrete selection set per
// possible object type.
type selectionSet struct {
	fields            []*planField
	byType            map[string]*selectionSet
	directSchedulable int
	deepSchedulable   bool
}

// forType returns the concrete selection set for obj.
func (s *selectionSet) forType(obj *objectType) *selectionSet {
	if s.byType != nil {
		return s.byType[obj.name]
	}
	return s
}

type planFieldKind uint8

const (
	fieldNormal planFieldKind = iota
	fieldTypename
)

// planField is one response key of a selection set.
type planField struct {
	kind        planFieldKind
	key         []byte
	alias       string
	name        string
	def         *fieldDef
	ast         *ast.Field
	args        any
	dynamicArgs bool
	sub         *selectionSet
	target      *objectType
	abstract    *abstractType
	exec        fieldExec
	schedulable bool
}

// fieldExec holds the executor functions used for a field within one plan.
// They start as copies of the fieldDef functions and are replaced by
// interceptor-wrapped versions when the executor has field interceptors.
type fieldExec struct {
	writeLeaf func(ctx context.Context, w *jsonw.Writer, parent, args any) error
	resolve   func(ctx context.Context, parent, args any) (any, error)
}

// compiler holds per-compilation state.
type compiler struct {
	s    *Schema
	e    *Executor
	doc  *ast.QueryDocument
	cond map[string]bool
	errs []*Error
}

// compilePlan flattens an operation into a plan. cond supplies the values of
// Boolean variables referenced by @skip and @include so that conditions are
// folded away.
func compilePlan(s *Schema, e *Executor, doc *ast.QueryDocument, op *ast.OperationDefinition, cond map[string]bool) (*plan, []*Error) {
	root := s.rootFor(op.Operation)
	if root == nil {
		return nil, []*Error{Errorf("schema does not define a %s root type", op.Operation).WithCode(CodeValidationFailed)}
	}
	c := &compiler{s: s, e: e, doc: doc, cond: cond}
	p := &plan{op: op, root: root}
	p.sel = c.compileSelection(root, nil, op.SelectionSet)
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	p.complexity = complexityOf(p.sel)
	p.depth = depthOf(p.sel)
	return p, nil
}

func (s *Schema) rootFor(op ast.Operation) *objectType {
	switch op {
	case ast.Query:
		return s.query
	case ast.Mutation:
		return s.mutation
	case ast.Subscription:
		return s.subscription
	}
	return nil
}

func (c *compiler) compileSelection(obj *objectType, abs *abstractType, sels ast.SelectionSet) *selectionSet {
	out := &selectionSet{}
	if abs == nil {
		out.fields = c.collect(obj, sels)
		out.directSchedulable, out.deepSchedulable = schedulability(out.fields)
		return out
	}
	names := make([]string, 0, len(abs.possible))
	for name := range abs.possible {
		names = append(names, name)
	}
	sort.Strings(names)
	out.byType = make(map[string]*selectionSet, len(names))
	for _, name := range names {
		concrete := c.compileSelection(abs.possible[name], nil, sels)
		out.byType[name] = concrete
		out.directSchedulable = max(out.directSchedulable, concrete.directSchedulable)
		out.deepSchedulable = out.deepSchedulable || concrete.deepSchedulable
	}
	return out
}

func schedulability(fields []*planField) (direct int, deep bool) {
	for _, f := range fields {
		if f.schedulable {
			direct++
			deep = true
		}
		if f.sub != nil && f.sub.deepSchedulable {
			deep = true
		}
	}
	return direct, deep
}

// fieldGroup gathers every occurrence of a response key in a selection set.
type fieldGroup struct {
	alias  string
	fields []*ast.Field
}

// collect implements CollectFields for a concrete object type.
func (c *compiler) collect(obj *objectType, sels ast.SelectionSet) []*planField {
	var groups []*fieldGroup
	index := make(map[string]*fieldGroup)
	visited := make(map[string]bool)
	c.collectInto(obj, sels, &groups, index, visited)

	out := make([]*planField, 0, len(groups))
	for _, g := range groups {
		if pf := c.buildField(obj, g); pf != nil {
			out = append(out, pf)
		}
	}
	return out
}

func (c *compiler) collectInto(obj *objectType, sels ast.SelectionSet, groups *[]*fieldGroup, index map[string]*fieldGroup, visited map[string]bool) {
	for _, sel := range sels {
		switch s := sel.(type) {
		case *ast.Field:
			if !c.included(s.Directives) {
				continue
			}
			key := s.Alias
			if key == "" {
				key = s.Name
			}
			g := index[key]
			if g == nil {
				g = &fieldGroup{alias: key}
				index[key] = g
				*groups = append(*groups, g)
			}
			g.fields = append(g.fields, s)
		case *ast.FragmentSpread:
			if !c.included(s.Directives) || visited[s.Name] {
				continue
			}
			visited[s.Name] = true
			frag := c.doc.Fragments.ForName(s.Name)
			if frag == nil || !c.applies(obj, frag.TypeCondition) {
				continue
			}
			c.collectInto(obj, frag.SelectionSet, groups, index, visited)
		case *ast.InlineFragment:
			if !c.included(s.Directives) {
				continue
			}
			if s.TypeCondition != "" && !c.applies(obj, s.TypeCondition) {
				continue
			}
			c.collectInto(obj, s.SelectionSet, groups, index, visited)
		}
	}
}

// included evaluates @skip and @include with literal or variant-bound
// Boolean values. @skip wins when both are present.
func (c *compiler) included(directives ast.DirectiveList) bool {
	for _, d := range directives {
		switch d.Name {
		case "skip":
			if c.boolArg(d) {
				return false
			}
		case "include":
			if !c.boolArg(d) {
				return false
			}
		}
	}
	return true
}

func (c *compiler) boolArg(d *ast.Directive) bool {
	arg := d.Arguments.ForName("if")
	if arg == nil {
		return false
	}
	switch arg.Value.Kind {
	case ast.BooleanValue:
		return arg.Value.Raw == "true"
	case ast.Variable:
		return c.cond[arg.Value.Raw]
	}
	return false
}

// applies reports whether a type condition matches obj: the condition names
// obj itself or an abstract type obj belongs to.
func (c *compiler) applies(obj *objectType, typeCondition string) bool {
	if typeCondition == obj.name {
		return true
	}
	for _, d := range c.s.ast.PossibleTypes[typeCondition] {
		if d == obj.def {
			return true
		}
	}
	return false
}

func (c *compiler) buildField(obj *objectType, g *fieldGroup) *planField {
	first := g.fields[0]
	pf := &planField{
		kind:  fieldNormal,
		key:   jsonw.EncodeKey(g.alias),
		alias: g.alias,
		name:  first.Name,
		ast:   first,
	}
	if first.Name == "__typename" {
		pf.kind = fieldTypename
		return pf
	}
	fd := obj.fields[first.Name]
	if fd == nil {
		c.errorf(first.Position, "Cannot query field %q on type %q.", first.Name, obj.name)
		return nil
	}
	pf.def = fd
	pf.exec = fieldExec{writeLeaf: fd.writeLeaf, resolve: fd.resolve}
	pf.schedulable = fd.schedulable
	if c.e != nil && len(c.e.fieldInterceptors) > 0 {
		pf.exec = c.e.interceptedExec(pf)
	}

	if fd.args != nil {
		if argsHaveVariables(first.Arguments) {
			pf.dynamicArgs = true
		} else {
			v, err := fd.args.decode(fieldArguments(first, nil))
			if err != nil {
				c.errorf(first.Position, "Invalid argument for field %s: %v", coordinate(obj.name, fd.name), err)
				return nil
			}
			pf.args = v
		}
	}

	if fd.leaf {
		return pf
	}

	var merged ast.SelectionSet
	if len(g.fields) == 1 {
		merged = first.SelectionSet
	} else {
		for _, f := range g.fields {
			merged = append(merged, f.SelectionSet...)
		}
	}
	named := c.s.ast.Types[fd.typ.Name()]
	switch named.Kind {
	case ast.Object:
		pf.target = c.s.objects[named.Name]
		pf.sub = c.compileSelection(pf.target, nil, merged)
	default:
		pf.abstract = c.s.abstracts[named.Name]
		pf.sub = c.compileSelection(nil, pf.abstract, merged)
	}
	return pf
}

func (c *compiler) errorf(pos *ast.Position, format string, args ...any) {
	e := Errorf(format, args...).WithCode(CodeValidationFailed)
	if pos != nil {
		e.Locations = []Location{{Line: pos.Line, Column: pos.Column}}
	}
	c.errs = append(c.errs, e)
}

func argsHaveVariables(args ast.ArgumentList) bool {
	for _, a := range args {
		if valueHasVariables(a.Value) {
			return true
		}
	}
	return false
}

func complexityOf(sel *selectionSet) int {
	if sel == nil {
		return 0
	}
	count := func(fields []*planField) int {
		n := 0
		for _, f := range fields {
			n += 1 + complexityOf(f.sub)
		}
		return n
	}
	if sel.byType == nil {
		return count(sel.fields)
	}
	most := 0
	for _, concrete := range sel.byType {
		most = max(most, count(concrete.fields))
	}
	return most
}

// condVariables returns the sorted names of Boolean variables used by @skip
// or @include anywhere in the document.
func condVariables(doc *ast.QueryDocument) []string {
	seen := make(map[string]bool)
	var visit func(sels ast.SelectionSet)
	visitDirectives := func(ds ast.DirectiveList) {
		for _, d := range ds {
			if d.Name != "skip" && d.Name != "include" {
				continue
			}
			if arg := d.Arguments.ForName("if"); arg != nil && arg.Value.Kind == ast.Variable {
				seen[arg.Value.Raw] = true
			}
		}
	}
	visit = func(sels ast.SelectionSet) {
		for _, sel := range sels {
			switch s := sel.(type) {
			case *ast.Field:
				visitDirectives(s.Directives)
				visit(s.SelectionSet)
			case *ast.FragmentSpread:
				visitDirectives(s.Directives)
			case *ast.InlineFragment:
				visitDirectives(s.Directives)
				visit(s.SelectionSet)
			}
		}
	}
	for _, op := range doc.Operations {
		visit(op.SelectionSet)
	}
	for _, frag := range doc.Fragments {
		visit(frag.SelectionSet)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// variantKey folds the values of the conditional variables into a bitmask.
func variantKey(condVars []string, vars map[string]any) (uint16, map[string]bool) {
	var key uint16
	values := make(map[string]bool, len(condVars))
	for i, name := range condVars {
		b, _ := vars[name].(bool)
		values[name] = b
		if b {
			key |= 1 << i
		}
	}
	return key, values
}

// planFor returns the plan for op under the given variables, compiling and
// caching it on first use.
func (d *docEntry) planFor(s *Schema, e *Executor, op *ast.OperationDefinition, vars map[string]any) (*plan, []*Error) {
	if len(d.condVars) > maxCondVars {
		cond := make(map[string]bool, len(d.condVars))
		for _, name := range d.condVars {
			cond[name], _ = vars[name].(bool)
		}
		return compilePlan(s, e, d.doc, op, cond)
	}
	variant, cond := variantKey(d.condVars, vars)
	key := planKey{op: op.Name, variant: variant}

	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.plans[key]; ok {
		return p, nil
	}
	p, errs := compilePlan(s, e, d.doc, op, cond)
	if errs != nil {
		return nil, errs
	}
	if d.plans == nil {
		d.plans = make(map[planKey]*plan, 1)
	}
	d.plans[key] = p
	return p, nil
}

// planCache is an LRU of parsed documents keyed by a 64-bit hash of the query
// text. Because two queries may hash alike, a hit is confirmed by comparing
// the stored query string, so a collision degrades to a miss.
type planCache struct {
	mu    sync.Mutex
	size  int
	items map[uint64]*list.Element
	lru   *list.List
	hash  func(string) uint64
}

type cacheItem struct {
	hash  uint64
	entry *docEntry
}

func newPlanCache(size int) *planCache {
	seed := maphash.MakeSeed()
	return &planCache{
		size:  size,
		items: make(map[uint64]*list.Element, size),
		lru:   list.New(),
		hash:  func(s string) uint64 { return maphash.String(seed, s) },
	}
}

// get returns the cached document for query, or nil.
func (c *planCache) get(query string) *docEntry {
	if c == nil || c.size <= 0 {
		return nil
	}
	h := c.hash(query)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[h]
	if !ok {
		return nil
	}
	item := el.Value.(*cacheItem)
	if item.entry.query != query {
		return nil
	}
	c.lru.MoveToFront(el)
	return item.entry
}

// put stores entry, evicting the least recently used document when full.
func (c *planCache) put(entry *docEntry) {
	if c == nil || c.size <= 0 {
		return
	}
	h := c.hash(entry.query)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[h]; ok {
		el.Value.(*cacheItem).entry = entry
		c.lru.MoveToFront(el)
		return
	}
	for c.lru.Len() >= c.size {
		oldest := c.lru.Back()
		c.lru.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheItem).hash)
	}
	c.items[h] = c.lru.PushFront(&cacheItem{hash: h, entry: entry})
}

// len reports the number of cached documents.
func (c *planCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}
