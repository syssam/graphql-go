package graphql

import (
	"container/list"
	"context"
	"hash/maphash"
	"math"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/syssam/graphql-go/internal/jsonw"
	"github.com/vektah/gqlparser/v2/ast"
)

// maxCondVars bounds the number of Boolean variables referenced by @skip and
// @include for which plan variants are cached.
const maxCondVars = 16

// maxPlanVariants bounds the plans kept for one document. Sixteen conditional
// variables are 65 536 variants of one query text, and a client chooses which
// to ask for; a variant past the bound is compiled for its request and not
// kept. Six variables explored in full fit, which is more than a client that
// toggles a few fragments ever sends.
const maxPlanVariants = 64

// docEntry is a parsed and validated document together with the plans
// compiled from it, one per (operation, skip/include variant).
type docEntry struct {
	query    string
	doc      *ast.QueryDocument
	condVars []string

	mu    sync.Mutex
	plans map[planKey]*plan
	// refused holds the metrics of the variants a depth or complexity limit
	// refused, which is the whole answer for them: kept, a repeat of a refused
	// request costs a map lookup rather than the walk that measured it. It
	// counts toward maxPlanVariants with plans.
	refused map[planKey]planMetrics
	// unkept holds the variants the cache's byte budget had no room for. A
	// plan it refused once it refuses again, since what the document is
	// charged only grows, so those are compiled per request, outside mu,
	// rather than under it each time to be told the same. It counts toward
	// maxPlanVariants with plans and refused.
	unkept map[planKey]struct{}
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
	// size estimates the bytes the plan retains, which is what the cache
	// charges for keeping it: see compiler.size.
	size int64
}

// What one of each costs a plan to hold. A selection set under an abstract
// parent has one entry per possible type; a map entry is its key and value
// with the bucket's share of overhead.
var (
	planFieldBytes    = int64(reflect.TypeFor[planField]().Size())
	selectionSetBytes = int64(reflect.TypeFor[selectionSet]().Size())
	authSiteBytes     = int64(reflect.TypeFor[AuthSite]().Size())
)

const (
	byTypeEntryBytes = 48
	// interceptedBytes is the closures interceptedExec builds for a field.
	interceptedBytes = 112
)

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
	// freshArgs narrows dynamicArgs: the arguments are literals and args is
	// their decoded value, to be copied for each request rather than decoded
	// again (argsCopier). It sits in dynamicArgs's padding, so the struct is
	// no larger for it.
	freshArgs   bool
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
	// the field declares no requirement and has no argument site. An argument
	// site synthesizes a zero-requirement output site here purely so the
	// field routes into enforceAuth, since the argument sites are found by
	// offset from this one. Returning something instance-guarded does not: an
	// instance check is routed by instanceSiteBit in argSites and never
	// through authIdx, so such a field stays at -1 unless it has one of the
	// other two. An integer compare on a field already in cache is what
	// keeps authorization free for fields that declare nothing.
	authIdx int32

	// argSites packs two things into the word beside authIdx, because
	// planField is exactly full at 176 bytes: an added int32 or even an added
	// bool measures 184, crossing a size class that runSubscriptionEvent pays
	// per event (`f := *src` copies a planField), and a slice header here
	// measured 200. The low bits count this field's argument sites, which the
	// plan's AuthShape stores contiguously after the field's output site so
	// enforceAuth walks them by offset from authIdx. instanceSiteBit records
	// only that the field returns something instance-guarded: writeList and
	// writeComposite route on that bit alone and never reach an instance site
	// through authIdx, so there is no index to derive.
	argSites int32
}

const (
	// instanceSiteBit rides above any plausible argument count: a field's
	// arguments are bounded by its SDL definition.
	instanceSiteBit int32 = 1 << 30
	argSiteMask     int32 = instanceSiteBit - 1
)

func (f *planField) argSiteCount() int32 { return f.argSites & argSiteMask }

func (f *planField) hasInstanceSite() bool { return f.argSites&instanceSiteBit != 0 }

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
	fragID map[string]int32

	// size adds up what the plan being built will retain: every plan field
	// and selection set once, however many parents share it. A plan is one
	// field per possible type of everything it selects on an interface, so
	// this is not a function of the query text -- a few kilobytes of aliases
	// on an interface with a thousand implementers is two hundred megabytes
	// -- and the text is what the cache counted.
	size int64

	// skipped holds what @skip and @include left out of this variant, each
	// with the concrete type it was being collected for. Only compilePlan
	// asks for it: the metrics walk shares collectInto and decodes nothing.
	trackSkips bool
	skipped    []skippedSelection
}

type skippedSelection struct {
	obj *objectType
	sel ast.Selection
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
//
// A fragment spread is the fragment it names, not the place it is written:
// collecting it yields the fragment's own nodes wherever it sits. Identified
// by spread node, one fragment under a thousand fields was a thousand
// selections, each collected and each compiled into its own copy. A spread
// this variant excludes keeps its own identity, so that every one of them is
// still met by a collect, which is where checkSkipped learns of it.
func (c *compiler) selFingerprint(sels ast.SelectionSet) string {
	var b []byte
	for _, s := range sels {
		if spread, ok := s.(*ast.FragmentSpread); ok && c.included(spread.Directives) {
			// Not a base-36 digit, so no node's number reads as a fragment's.
			b = append(b, '#')
			b = strconv.AppendInt(b, int64(c.fragNum(spread.Name)), 36)
		} else {
			b = strconv.AppendInt(b, int64(c.nodeNum(s)), 36)
		}
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

// fragNum numbers a fragment by name on first sight.
func (c *compiler) fragNum(name string) int32 {
	if n, ok := c.fragID[name]; ok {
		return n
	}
	if c.fragID == nil {
		c.fragID = make(map[string]int32)
	}
	n := int32(len(c.fragID))
	c.fragID[name] = n
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
		memo:       make(map[selKey]*selectionSet),
		nodeID:     make(map[ast.Selection]int32),
		trackSkips: true,
	}
	p := &plan{op: op, root: root}
	p.sel = c.compileSelection(root, nil, op.SelectionSet)
	c.checkSkipped()
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	p.shape = buildAuthShape(p.root, p.sel)
	p.size = c.size
	if p.shape != nil {
		p.size += authSiteBytes * int64(len(p.shape.sites))
	}
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
	c.size += selectionSetBytes
	if abs == nil {
		out.fields = c.collect(obj, sels)
		c.size += 8 * int64(cap(out.fields))
		out.directSchedulable, out.deepSchedulable = schedulability(out.fields)
		c.memo[key] = out
		return out
	}
	names := make([]string, 0, len(abs.possible))
	for name := range abs.possible {
		names = append(names, name)
	}
	slices.Sort(names)
	out.byType = make(map[string]*selectionSet, len(names))
	c.size += byTypeEntryBytes * int64(len(names))
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
				c.skip(obj, s)
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
			if !c.included(s.Directives) {
				c.skip(obj, s)
				continue
			}
			if visited[s.Name] {
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
				c.skip(obj, s)
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
	c.size += planFieldBytes + int64(cap(pf.key))
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
	if c.e != nil && c.e.cost != nil && c.e.cost.Actual && !isIntrospection(obj.name, fd.name) {
		// Held to what the request's 32-bit total can add without the
		// conversion itself wrapping.
		pf.costWeight = min(max(c.e.cost.weight(coordinate(obj.name, fd.name)), math.MinInt32), math.MaxInt32)
	}
	if c.e != nil && len(c.e.fieldInterceptors) > 0 {
		pf.exec = c.e.interceptedExec(pf)
		c.size += interceptedBytes
	}

	if fd.args != nil {
		if argsHaveVariables(first.Arguments) {
			pf.dynamicArgs = true
		} else {
			v, ok := c.literalArgs(obj, fd, first)
			if !ok {
				return nil
			}
			// The plan is shared by every request for this query, so each
			// request is given arguments of its own unless a copy of these
			// would share nothing with them: by copying them where that is
			// enough, by decoding again where it is not. Either way the value
			// decoded here stays on the plan, for Selection to show.
			pf.args = v
			switch fd.args.copier.sharing(reflect.ValueOf(v).Elem()) {
			case argsCopied:
				pf.dynamicArgs, pf.freshArgs = true, true
			case argsDecoded:
				pf.dynamicArgs = true
			}
			c.size += int64(fd.args.goType.Size())
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

// literalArgs decodes arguments written without a variable, recording the
// error when they do not decode. gqlparser checks a literal's kind and not its
// value, so this is where an Int out of range or a custom scalar's own
// refusal is found.
func (c *compiler) literalArgs(obj *objectType, fd *fieldDef, f *ast.Field) (any, bool) {
	raw, aerr := fieldArguments(f, nil)
	v, err := fd.args.decode(raw)
	if err == nil {
		err = aerr
	}
	if err != nil {
		c.errorf(f.Position, "Invalid argument for field %s: %v", coordinate(obj.name, fd.name), err)
		return nil, false
	}
	return v, true
}

func (c *compiler) skip(obj *objectType, sel ast.Selection) {
	if c.trackSkips {
		c.skipped = append(c.skipped, skippedSelection{obj: obj, sel: sel})
	}
}

// checkSkipped decodes the literal arguments of everything a directive left
// out of this variant, as buildField does for what it kept. Whether a literal
// is acceptable is a fact about the document: checked only where the plan
// uses it, the same query text was refused with one value of a variable and
// ran with the other. It walks as the compile would have, by concrete type,
// so the errors are the ones the variant that includes the field reports.
func (c *compiler) checkSkipped() {
	if len(c.skipped) == 0 {
		return
	}
	w := &skippedWalk{
		c:      c,
		fields: make(map[skippedField]bool),
		frags:  make(map[skippedFragment]bool),
		known:  make(map[skippedError]bool, len(c.errs)),
	}
	// A fragment spread both with and without a condition is compiled as
	// well as walked here, and its error is one error.
	for _, e := range c.errs {
		w.known[skippedErrorOf(e)] = true
	}
	for _, s := range c.skipped {
		w.selections(s.obj, ast.SelectionSet{s.sel})
	}
}

type skippedField struct {
	obj *objectType
	f   *ast.Field
}

type skippedFragment struct {
	obj  *objectType
	name string
}

type skippedError struct {
	message string
	at      Location
}

func skippedErrorOf(e *Error) skippedError {
	k := skippedError{message: e.Message}
	if len(e.Locations) > 0 {
		k.at = e.Locations[0]
	}
	return k
}

// skippedWalk visits each field of each concrete type once, which bounds it
// as the compile's memo bounds the compile: by document size times type count.
type skippedWalk struct {
	c      *compiler
	fields map[skippedField]bool
	frags  map[skippedFragment]bool
	known  map[skippedError]bool
}

func (w *skippedWalk) selections(obj *objectType, sels ast.SelectionSet) {
	for _, sel := range sels {
		switch s := sel.(type) {
		case *ast.Field:
			w.field(obj, s)
		case *ast.FragmentSpread:
			k := skippedFragment{obj: obj, name: s.Name}
			frag := w.c.doc.Fragments.ForName(s.Name)
			if frag == nil || w.frags[k] || !w.c.applies(obj, frag.TypeCondition) {
				continue
			}
			w.frags[k] = true
			w.selections(obj, frag.SelectionSet)
		case *ast.InlineFragment:
			if s.TypeCondition != "" && !w.c.applies(obj, s.TypeCondition) {
				continue
			}
			w.selections(obj, s.SelectionSet)
		}
	}
}

func (w *skippedWalk) field(obj *objectType, f *ast.Field) {
	k := skippedField{obj: obj, f: f}
	fd := obj.fields[f.Name]
	if fd == nil || w.fields[k] {
		return
	}
	w.fields[k] = true
	if fd.args != nil && !argsHaveVariables(f.Arguments) {
		before := len(w.c.errs)
		if _, ok := w.c.literalArgs(obj, fd, f); !ok {
			if e := skippedErrorOf(w.c.errs[before]); w.known[e] {
				w.c.errs = w.c.errs[:before]
			} else {
				w.known[e] = true
			}
		}
	}
	if fd.leaf {
		return
	}
	named := w.c.s.ast.Types[fd.typ.Name()]
	if named.Kind == ast.Object {
		w.selections(w.c.s.objects[named.Name], f.SelectionSet)
		return
	}
	abs := w.c.s.abstracts[named.Name]
	if abs == nil {
		return
	}
	names := make([]string, 0, len(abs.possible))
	for name := range abs.possible {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		w.selections(abs.possible[name], f.SelectionSet)
	}
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

// argsSharing is what has to happen for a request to be handed arguments
// decoded from literals. A resolver takes its argument struct by value, and
// that is a copy of the struct only: a nullable scalar is bound as a pointer,
// a list as a slice, an input object behind a pointer, and each would then be
// one value for every request the plan serves, which a resolver clamping its
// own page size or sorting its own list changes for the next.
type argsSharing uint8

const (
	// argsShared: a copy shares nothing, so the plan's value serves them all.
	argsShared argsSharing = iota
	// argsCopied: all a copy shares is what top-level pointers to plain
	// values point at, and argsCopier gives a request its own.
	argsCopied
	// argsDecoded: anything else, decoded again for each request.
	argsDecoded
)

// argsCopier copies an argument struct A together with what its pointers to
// plain values point at, in one allocation: the struct and a slot for each
// pointee are fields of one block, and the copy's pointers point into it.
// first: 5 into a *int is the commonest argument there is, and decoding it
// again for every request was five allocations -- the argument map, the
// number, the struct, the int -- where this is one.
//
// It is built once for a binding, for the fields a copy can be trusted with:
// exported, at the top level of A, and pointing at a scalar's or an enum's
// Go type that holds no memory of its own, so that copying the pointee
// copies all of it.
type argsCopier struct {
	block reflect.Type
	// fields are the indexes in A of those pointers; slot i of the block,
	// after A itself, is what fields[i] points at.
	fields []int
}

// newArgsCopier returns nil when A has no such field. inputObject reports
// that a Go type is bound to an input object.
func newArgsCopier(tA reflect.Type, inputObject func(reflect.Type) bool) *argsCopier {
	c := &argsCopier{}
	block := []reflect.StructField{{Name: "Args", Type: tA}}
	for i := range tA.NumField() {
		f := tA.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Pointer || !plainValue(f.Type.Elem()) {
			continue
		}
		if f.Type.Elem().Kind() == reflect.Struct && inputObject(f.Type.Elem()) {
			continue
		}
		c.fields = append(c.fields, i)
		block = append(block, reflect.StructField{Name: "Slot" + strconv.Itoa(len(c.fields)), Type: f.Type.Elem()})
	}
	if len(c.fields) == 0 {
		return nil
	}
	c.block = reflect.StructOf(block)
	return c
}

// plainValue reports whether a value of type t holds everything it is, so
// that a copy of it shares nothing. A string does share its bytes and cannot
// be written through. Neither can the *time.Location inside a time.Time,
// which is why that one struct is not looked into.
func plainValue(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	case reflect.Array:
		return plainValue(t.Elem())
	case reflect.Struct:
		if t == timeType {
			return true
		}
		for i := range t.NumField() {
			if !plainValue(t.Field(i).Type) {
				return false
			}
		}
		return true
	}
	return false
}

var timeType = reflect.TypeFor[time.Time]()

// sharing decides for one decoded value, at plan compile. It is asked of the
// value rather than the type, so an optional argument the query leaves out --
// a nil pointer, a nil slice -- costs a request nothing. A nil copier is a
// binding with nothing it can copy; for that it is shared or decoded.
//
// One pointer found in two fields is decoded again: a setter written by hand
// may have put it in both, and a copy would give each field a pointee of its
// own where the resolver was written against one.
func (c *argsCopier) sharing(args reflect.Value) argsSharing {
	copied := false
	next := 0
	var seen []uintptr
	for i := range args.NumField() {
		f := args.Field(i)
		if c != nil && next < len(c.fields) && c.fields[next] == i {
			next++
			if !f.IsNil() {
				p := f.Pointer()
				if slices.Contains(seen, p) {
					return argsDecoded
				}
				seen = append(seen, p)
				copied = true
			}
			continue
		}
		if sharedInside(f) {
			return argsDecoded
		}
	}
	if copied {
		return argsCopied
	}
	return argsShared
}

// fresh is a request's own copy of args, which is the *A the plan decoded.
func (c *argsCopier) fresh(args any) any {
	from := reflect.ValueOf(args).Elem()
	block := reflect.New(c.block).Elem()
	to := block.Field(0)
	to.Set(from)
	for i, field := range c.fields {
		p := from.Field(field)
		if p.IsNil() {
			continue
		}
		slot := block.Field(1 + i)
		slot.Set(p.Elem())
		to.Field(field).Set(slot.Addr())
	}
	return to.Addr().Interface()
}

// sharedInside reports whether v holds memory that a copy of v would still
// point at. An interface is taken to whenever it holds anything: what is
// behind it is whatever a scalar's unmarshal chose to return.
func sharedInside(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Interface:
		return !v.IsNil()
	case reflect.Slice:
		// An empty slice with no capacity has nothing to write into.
		return v.Cap() > 0
	case reflect.Struct:
		if v.Type() == timeType {
			return false
		}
		for i := range v.NumField() {
			if sharedInside(v.Field(i)) {
				return true
			}
		}
	case reflect.Array:
		for i := range v.Len() {
			if sharedInside(v.Index(i)) {
				return true
			}
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

// variantKey folds the values of the conditional variables into a bitmask,
// and reports whether the request sent any of them as null.
func variantKey(condVars []string, vars map[string]any) (key uint16, null bool) {
	for i, name := range condVars {
		v, sent := vars[name]
		if b, _ := v.(bool); b {
			key |= 1 << i
		} else if sent && v == nil {
			null = true
		}
	}
	return key, null
}

// nullCondition returns the error for the first @skip or @include of op whose
// variable the request sent as null. The directives take a Boolean!, and a
// variable with a default may be declared nullable in that position, so
// validation cannot refuse it; folded into the variant as "not true" it
// answered the request as though the client had chosen. It runs only once
// variantKey has seen a null, and it walks the one operation because a
// document's other operations may use the same variable name in a directive.
func nullCondition(doc *ast.QueryDocument, op *ast.OperationDefinition, vars map[string]any) *Error {
	visited := make(map[string]bool)
	check := func(ds ast.DirectiveList) *Error {
		for _, d := range ds {
			if d.Name != "skip" && d.Name != "include" {
				continue
			}
			arg := d.Arguments.ForName("if")
			if arg == nil || arg.Value.Kind != ast.Variable {
				continue
			}
			if v, sent := vars[arg.Value.Raw]; !sent || v != nil {
				continue
			}
			e := Errorf("Variable \"$%s\" must not be null: it is the \"if\" argument of @%s, of non-null type \"Boolean!\".", arg.Value.Raw, d.Name).
				WithCode(CodeBadUserInput)
			if pos := arg.Value.Position; pos != nil {
				e.Locations = []Location{{Line: pos.Line, Column: pos.Column}}
			}
			return e
		}
		return nil
	}
	var visit func(sels ast.SelectionSet) *Error
	visit = func(sels ast.SelectionSet) *Error {
		for _, sel := range sels {
			var ds ast.DirectiveList
			var sub ast.SelectionSet
			switch s := sel.(type) {
			case *ast.Field:
				ds, sub = s.Directives, s.SelectionSet
			case *ast.InlineFragment:
				ds, sub = s.Directives, s.SelectionSet
			case *ast.FragmentSpread:
				ds = s.Directives
				if frag := doc.Fragments.ForName(s.Name); frag != nil && !visited[s.Name] {
					visited[s.Name] = true
					sub = frag.SelectionSet
				}
			}
			if err := check(ds); err != nil {
				return err
			}
			if err := visit(sub); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(op.SelectionSet)
}

// condValues materializes the conditional variables a compile needs. A plan
// cache hit never calls it, so a request that reuses a plan builds no map.
func condValues(condVars []string, vars map[string]any) map[string]bool {
	if len(condVars) == 0 {
		return nil
	}
	values := make(map[string]bool, len(condVars))
	for _, name := range condVars {
		values[name], _ = vars[name].(bool)
	}
	return values
}

// planUncacheable reports that this document has too many @skip/@include
// variables for the variant cache, so every request recompiles its plan.
func (d *docEntry) planUncacheable() bool { return len(d.condVars) > maxCondVars }

// planFor returns the plan for this operation and variant, reporting whether
// it was already compiled. The caller must not inspect d.plans itself: it is
// written under d.mu, and reading it unlocked is a data race that concurrent
// requests for the same query will hit.
func (d *docEntry) planFor(s *Schema, e *Executor, op *ast.OperationDefinition, vars map[string]any) (*plan, bool, []*Error) {
	variant, null := variantKey(d.condVars, vars)
	if null {
		if err := nullCondition(d.doc, op, vars); err != nil {
			return nil, false, []*Error{err}
		}
	}
	if d.planUncacheable() {
		return d.compile(s, e, op, condValues(d.condVars, vars), planKey{}, false)
	}
	key := planKey{op: op.Name, variant: variant}

	d.mu.Lock()
	if p, ok := d.plans[key]; ok {
		d.mu.Unlock()
		return p, true, nil
	}
	if m, ok := d.refused[key]; ok {
		d.mu.Unlock()
		return nil, false, []*Error{e.rejectByMetrics(m)}
	}
	// A variant that will not be kept is compiled outside the lock, as an
	// uncacheable document's is: it is compiled again on every request for
	// it, and under the lock each of those held up every other request for
	// this document, the cached variants included.
	if _, unkept := d.unkept[key]; unkept || len(d.plans)+len(d.refused)+len(d.unkept) >= maxPlanVariants {
		d.mu.Unlock()
		return d.compile(s, e, op, condValues(d.condVars, vars), planKey{}, false)
	}
	defer d.mu.Unlock()
	return d.compile(s, e, op, condValues(d.condVars, vars), key, true)
}

// compile runs the pre-compile guard and then compiles. store is false for the
// uncached path taken when the document has more conditional variables than
// maxCondVars. e is never nil on any path that reaches here: planFor's only
// callers (exec.go, subscription.go) pass the receiver Executor, and every
// direct plan_test.go caller of compilePlan supplies a real *Executor too.
func (d *docEntry) compile(s *Schema, e *Executor, op *ast.OperationDefinition, cond map[string]bool, key planKey, store bool) (*plan, bool, []*Error) {
	m := limitedMetrics(s, d.doc, op, cond, e.maxComplexity, e.maxDepth)
	if err := e.rejectByMetrics(m); err != nil {
		if store {
			if d.refused == nil {
				d.refused = make(map[planKey]planMetrics, 1)
			}
			d.refused[key] = m
		}
		return nil, false, []*Error{err}
	}
	p, errs := compilePlan(s, e, d.doc, op, cond)
	if errs != nil {
		return nil, false, errs
	}
	p.complexity = m.complexity
	p.depth = m.depth
	// A plan is kept only while the cache's byte budget has room for what it
	// retains; planFor has already held the document to maxPlanVariants.
	if store && !e.cache.charge(d, p.size) {
		if d.unkept == nil {
			d.unkept = make(map[planKey]struct{}, 1)
		}
		d.unkept[key] = struct{}{}
		store = false
	}
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
	// bytes is what this item is charged against the budget: the query text
	// for the document, and for each plan kept what the plan retains.
	bytes int64
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
		c.used += n - item.bytes
		item.entry, item.bytes = entry, n
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
	c.items[h] = c.lru.PushFront(&cacheItem{hash: h, entry: entry, bytes: n})
	c.used += n
}

// charge accounts for a plan of n bytes kept by entry and reports whether it
// may be kept. The document itself is counted as its query text, which is
// what there is to count before it is parsed, and is a small part of what
// the document goes on to hold: a plan's size depends on the schema as much
// as on the text, so it is counted when it exists, as what it is. Other
// documents are evicted to make room, least recently used first; the plan is
// refused only when this document's own charge would pass the whole budget,
// and then it is compiled for each request that needs it.
//
// An entry the cache does not hold -- evicted since, or never cacheable --
// lives no longer than the requests using it, so its plans are not counted.
func (c *planCache) charge(entry *docEntry, n int64) bool {
	if c == nil || c.size <= 0 {
		return true
	}
	h := c.hash(entry.query)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[h]
	if !ok {
		return true
	}
	item := el.Value.(*cacheItem)
	if item.entry != entry {
		return true
	}
	if c.maxBytes > 0 {
		if item.bytes+n > c.maxBytes {
			return false
		}
		// At the front it is never the oldest, so the loop cannot evict it.
		c.lru.MoveToFront(el)
		for c.used+n > c.maxBytes && c.lru.Len() > 1 {
			c.removeOldest()
		}
	}
	item.bytes += n
	c.used += n
	return true
}

func (c *planCache) removeOldest() {
	oldest := c.lru.Back()
	item := oldest.Value.(*cacheItem)
	c.lru.Remove(oldest)
	delete(c.items, item.hash)
	c.used -= item.bytes
}

// bytes reports what the cache currently holds against its budget.
func (c *planCache) bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// occupancy reports the number of cached documents and what they are charged,
// read together so the two describe the same moment.
func (c *planCache) occupancy() (int, int64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.used
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
