package graphql

import (
	"container/list"
	"context"
	"hash/maphash"
	"slices"
	"sort"
	"strconv"
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
	shape      *AuthShape
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

	// costWeight is this field's contribution to the actual query cost,
	// resolved once here so the write path adds an integer instead of
	// looking up a schema coordinate. It stays zero unless actual cost is
	// enabled, which is what keeps the write path unchanged when it is not.
	costWeight int

	// authIdx indexes this field's site in the plan's AuthShape, or -1 when
	// the field declares no requirement and has no argument site. An integer
	// compare on a field already in cache is what keeps authorization free
	// for fields that declare nothing.
	authIdx int32

	// argSites counts this field's argument sites. They are stored
	// contiguously in the plan's AuthShape immediately after the field's own
	// output site -- shape.sites[authIdx+1 : authIdx+1+int(argSites)] -- so
	// this can stay a count instead of a slice: runSubscriptionEvent copies
	// a planField by value (`f := *src`) once per event, and a slice header
	// here pushed the struct from 176 to 200 bytes, into the next size
	// class, paid on every event.
	argSites int32
}

// fieldExec holds the executor functions used for a field within one plan.
// They start as copies of the fieldDef functions and are replaced by
// interceptor-wrapped versions when the executor has field interceptors, and
// by event-yielding versions for a subscription's per-event root field.
type fieldExec struct {
	writeLeaf func(ctx context.Context, w *jsonw.Writer, parent, args any, fc *FieldContext) error
	resolve   func(ctx context.Context, parent, args any, fc *FieldContext) (any, error)

	// resolveAny is writeLeaf's value-producing half, type-erased, for a
	// leaf field only. Redact needs the resolved value before it is written
	// so it can rewrite it, which writeLeaf's direct resolve-then-write does
	// not expose. It is set where this plan field's executor differs from
	// the shared fieldDef's -- an interceptor chain, or a subscription's
	// per-event substitute -- and nil otherwise, meaning fd.anyResolve is
	// exactly right. Leaving it nil there rather than wrapping fd.anyResolve
	// in a closure keeps plan compilation from allocating per field.
	resolveAny func(ctx context.Context, parent, args any, fc *FieldContext) (any, error)
}

// compiler holds per-compilation state.
type compiler struct {
	s    *Schema
	e    *Executor
	doc  *ast.QueryDocument
	cond map[string]bool
	errs []*Error

	// An interface with N implementers selected D levels deep expands to N^D
	// selection sets, because each concrete expansion can contain further
	// abstract fields. compileSelection is a pure function of its parent and
	// selection set for a fixed cond, and plan structures are read-only once
	// built, so identical calls share one result and the tree becomes a DAG.
	memo   map[selKey]*selectionSet
	nodeID map[ast.Selection]int32
}

// selKey identifies a compileSelection call. parent holds the *objectType for
// a concrete parent or the *abstractType for an abstract one; both are
// pointers, so the interface value is comparable.
type selKey struct {
	parent any
	sels   string
}

// selFingerprint identifies a selection set by the identities of the AST nodes
// in it. Slice identity will not do: buildField reuses first.SelectionSet when
// a response key has a single AST node but appends a fresh slice when it has
// more, so a query repeating a response key would miss a pointer-keyed memo at
// every level and expand exponentially regardless.
func (c *compiler) selFingerprint(sels ast.SelectionSet) string {
	var b []byte
	for _, s := range sels {
		b = strconv.AppendInt(b, int64(c.nodeNum(s)), 36)
		b = append(b, ',')
	}
	return string(b)
}

// nodeNum numbers an AST selection node on first sight. The document is cached
// and never mutated, so a node's identity is stable across every plan compiled
// from it.
func (c *compiler) nodeNum(s ast.Selection) int32 {
	if n, ok := c.nodeID[s]; ok {
		return n
	}
	n := int32(len(c.nodeID))
	c.nodeID[s] = n
	return n
}

// compilePlan flattens an operation into a plan. cond supplies the values of
// Boolean variables referenced by @skip and @include so that conditions are
// folded away.
func compilePlan(s *Schema, e *Executor, doc *ast.QueryDocument, op *ast.OperationDefinition, cond map[string]bool) (*plan, []*Error) {
	root := s.rootFor(op.Operation)
	if root == nil {
		return nil, []*Error{Errorf("schema does not define a %s root type", op.Operation).WithCode(CodeValidationFailed)}
	}
	c := &compiler{
		s: s, e: e, doc: doc, cond: cond,
		memo:   make(map[selKey]*selectionSet),
		nodeID: make(map[ast.Selection]int32),
	}
	p := &plan{op: op, root: root}
	p.sel = c.compileSelection(root, nil, op.SelectionSet)
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	p.shape = buildAuthShape(p.root, p.sel)
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
	key := selKey{parent: any(obj), sels: c.selFingerprint(sels)}
	if abs != nil {
		key.parent = any(abs)
	}
	if s, ok := c.memo[key]; ok {
		return s
	}

	out := &selectionSet{}
	if abs == nil {
		out.fields = c.collect(obj, sels)
		out.directSchedulable, out.deepSchedulable = schedulability(out.fields)
		c.memo[key] = out
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
	c.memo[key] = out
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
		// authIdx defaults to -1 (no site) by construction rather than
		// solely by shapeBuilder.field visiting it: a future change that
		// skips buildAuthShape would otherwise leave Go's zero value 0,
		// which points every unvisited field at site 0 instead of at none.
		authIdx: -1,
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
	if c.e != nil && c.e.cost != nil && c.e.cost.Actual {
		pf.costWeight = c.e.cost.weight(coordinate(obj.name, fd.name))
	}
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

// planUncacheable reports that this document has too many @skip/@include
// variables for the variant cache, so every request recompiles its plan.
func (d *docEntry) planUncacheable() bool { return len(d.condVars) > maxCondVars }

// planFor returns the plan for this operation and variant, reporting whether
// it was already compiled. The caller must not inspect d.plans itself: it is
// written under d.mu, and reading it unlocked is a data race that concurrent
// requests for the same query will hit.
func (d *docEntry) planFor(s *Schema, e *Executor, op *ast.OperationDefinition, vars map[string]any) (*plan, bool, []*Error) {
	if d.planUncacheable() {
		cond := make(map[string]bool, len(d.condVars))
		for _, name := range d.condVars {
			cond[name], _ = vars[name].(bool)
		}
		return d.compile(s, e, op, cond, planKey{}, false)
	}
	variant, cond := variantKey(d.condVars, vars)
	key := planKey{op: op.Name, variant: variant}

	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.plans[key]; ok {
		return p, true, nil
	}
	return d.compile(s, e, op, cond, key, true)
}

// compile runs the pre-compile guard and then compiles. store is false for the
// uncached path taken when the document has more conditional variables than
// maxCondVars. e is never nil on any path that reaches here: planFor's only
// callers (exec.go, subscription.go) pass the receiver Executor, and every
// direct plan_test.go caller of compilePlan supplies a real *Executor too.
func (d *docEntry) compile(s *Schema, e *Executor, op *ast.OperationDefinition, cond map[string]bool, key planKey, store bool) (*plan, bool, []*Error) {
	m := operationMetrics(s, d.doc, op, cond)
	if err := e.rejectByMetrics(m); err != nil {
		return nil, false, []*Error{err}
	}
	p, errs := compilePlan(s, e, d.doc, op, cond)
	if errs != nil {
		return nil, false, errs
	}
	p.complexity = m.complexity
	p.depth = m.depth
	if store {
		if d.plans == nil {
			d.plans = make(map[planKey]*plan, 1)
		}
		d.plans[key] = p
	}
	return p, false, nil
}

// planCache is an LRU of parsed documents keyed by a 64-bit hash of the query
// text. Because two queries may hash alike, a hit is confirmed by comparing
// the stored query string, so a collision degrades to a miss.
type planCache struct {
	mu       sync.Mutex
	size     int
	maxBytes int64
	used     int64
	items    map[uint64]*list.Element
	lru      *list.List
	hash     func(string) uint64
}

type cacheItem struct {
	hash  uint64
	entry *docEntry
}

func newPlanCache(size int, maxBytes int64) *planCache {
	seed := maphash.MakeSeed()
	return &planCache{
		size:     size,
		maxBytes: maxBytes,
		items:    make(map[uint64]*list.Element, size),
		lru:      list.New(),
		hash:     func(s string) uint64 { return maphash.String(seed, s) },
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

// put stores entry, evicting least recently used documents until both the
// entry count and the byte budget hold.
func (c *planCache) put(entry *docEntry) {
	if c == nil || c.size <= 0 {
		return
	}
	n := int64(len(entry.query))
	// A query larger than the whole budget can never fit, and evicting to make
	// room for it would only discard documents that do.
	if c.maxBytes > 0 && n > c.maxBytes {
		return
	}
	h := c.hash(entry.query)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[h]; ok {
		item := el.Value.(*cacheItem)
		c.used += n - int64(len(item.entry.query))
		item.entry = entry
		c.lru.MoveToFront(el)
		// A replacement can grow its slot past the budget. The replaced entry is
		// now at the front and fits on its own, so this stops before reaching it.
		for c.maxBytes > 0 && c.used > c.maxBytes && c.lru.Len() > 1 {
			c.removeOldest()
		}
		return
	}
	for c.lru.Len() > 0 && (c.lru.Len() >= c.size || (c.maxBytes > 0 && c.used+n > c.maxBytes)) {
		c.removeOldest()
	}
	c.items[h] = c.lru.PushFront(&cacheItem{hash: h, entry: entry})
	c.used += n
}

func (c *planCache) removeOldest() {
	oldest := c.lru.Back()
	item := oldest.Value.(*cacheItem)
	c.lru.Remove(oldest)
	delete(c.items, item.hash)
	c.used -= int64(len(item.entry.query))
}

// bytes reports the query text the cache currently holds.
func (c *planCache) bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
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
