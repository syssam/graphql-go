package graphql

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// writeObject writes the selected fields of val, an object of type obj, as a
// JSON object. It returns false when a non-null field failed and the object
// itself must become null; the writer is then rewound to where the object
// started so the caller can write null in its place.
func (st *execState) writeObject(ctx context.Context, w *jsonw.Writer, obj *objectType, sel *selectionSet, val any, path *pathNode, serial bool) bool {
	sel = sel.forType(obj)
	mark := w.Mark()
	w.BeginObject()
	if !serial && sel.directSchedulable >= 2 && st.e.sem != nil {
		if !st.writeFieldsConcurrent(ctx, w, obj, sel, val, path) {
			w.Rewind(mark)
			return false
		}
	} else {
		for _, f := range sel.fields {
			if !st.writeField(ctx, w, obj, f, val, path) {
				w.Rewind(mark)
				return false
			}
		}
	}
	w.EndObject()
	return true
}

// writeField writes one response key. It returns false when the field is
// non-null and failed, or when the response limit has been passed; the caller
// must propagate either.
func (st *execState) writeField(ctx context.Context, w *jsonw.Writer, obj *objectType, f *planField, parent any, path *pathNode) bool {
	fm := w.Mark()
	w.Key(f.key)
	if st.writeFieldValue(ctx, w, obj, f, parent, path) {
		return true
	}
	// Past the response limit a null is no cheaper to keep than the field was:
	// the response is discarded, and nulling here would let every enclosing
	// list carry on to its next element.
	if f.kind == fieldTypename || (f.def != nil && f.def.typ.NonNull) || w.LimitExceeded() {
		return false
	}
	w.Rewind(fm)
	w.Key(f.key)
	w.Null()
	return true
}

// writeFieldValue resolves and writes the value of f. On failure it records
// the error and returns false without cleaning up partial output; callers
// rewind.
func (st *execState) writeFieldValue(ctx context.Context, w *jsonw.Writer, obj *objectType, f *planField, parent any, path *pathNode) bool {
	// An unguarded __typename reads only the planField it is already on, so it
	// pays no load from execState on the path every Apollo client exercises.
	if f.kind == fieldTypename && f.authIdx < 0 {
		w.String(obj.name)
		return true
	}
	// -1 on a field that declares nothing, so the ordinary path pays one
	// compare on a struct already in cache.
	if st.decision != nil && f.authIdx >= 0 {
		if done, ok := st.enforceAuth(ctx, w, f, path); done {
			return ok
		}
	}
	if f.kind == fieldTypename {
		w.String(obj.name)
		return true
	}
	if w.OverLimit() {
		return false
	}
	if err := ctx.Err(); err != nil {
		st.recordCancellation(ctx, err)
		return false
	}
	// Zero unless actual cost is enabled, so this is one compare and no
	// atomic on the ordinary path. Placed after the limit and cancellation
	// checks so a field that never resolves is never counted.
	if f.costWeight != 0 {
		st.actualCost.Add(int32(f.costWeight))
	}
	fd := f.def
	args := f.args
	if f.dynamicArgs {
		v, err := fd.args.decode(fieldArguments(f.ast, st.vars))
		if err != nil {
			st.fieldError(ctx, Errorf("Invalid argument for field %s: %v", coordinate(obj.name, fd.name), err).WithCode(CodeBadUserInput), path, f)
			return false
		}
		args = v
	}

	if fd.leaf {
		var err error
		if o := st.authOutcome(f); o.act == actionRedact {
			err = st.callLeafRedacted(ctx, w, f, parent, args, path, o.redact)
		} else {
			err = st.callLeaf(ctx, w, f, parent, args, path)
		}
		if err == nil {
			return true
		}
		st.fieldError(ctx, err, path, f)
		var soft *elementErrors
		return errors.As(err, &soft)
	}

	v, err := st.callResolve(ctx, f, parent, args, path)
	if err != nil {
		st.fieldError(ctx, err, path, f)
		return false
	}
	return st.writeValue(ctx, w, v, fd.typ, fd.shape, f, &pathNode{parent: path, key: f.alias})
}

// fieldContext builds the FieldContext for a field and attaches it to the
// context only where something could read it back: a resolver may call
// FieldFrom or PathFrom, a pure accessor takes no context at all. An
// interceptor receives it as an argument either way.
func (st *execState) fieldContext(ctx context.Context, f *planField, parent, args any, path *pathNode) (context.Context, *FieldContext) {
	fd := f.def
	if fd.pure && len(st.e.fieldInterceptors) == 0 {
		return ctx, nil
	}
	fc := &FieldContext{Field: fd.def, Object: fd.object.def, Args: args, Parent: parent, field: f, pathParent: path, alias: f.alias}
	if fd.pure {
		return ctx, fc
	}
	return withField(ctx, fc), fc
}

// fieldInfo builds a FieldInfo from the plan, allocating nothing: everything
// it reads already lives on f.def.
func (st *execState) fieldInfo(f *planField, path *pathNode) FieldInfo {
	fd := f.def
	return FieldInfo{Object: fd.object.name, Field: fd.name, Alias: f.alias, pathParent: path}
}

// observerContexts is how many observers' contexts a field keeps on the stack
// before it needs a heap slice. Each observer's EndField must get back the
// context its own BeginField returned, not the innermost one, so every one is
// kept; registering more observers than this is unusual enough that the
// allocation is acceptable.
const observerContexts = 4

// callLeaf invokes a leaf executor with panic protection and a FieldContext.
func (st *execState) callLeaf(ctx context.Context, w *jsonw.Writer, f *planField, parent, args any, path *pathNode) (err error) {
	ctx, fc := st.fieldContext(ctx, f, parent, args, path)
	// The observer's deferred EndField must be registered before the recovery
	// defer below, so it runs after recovery has converted a panic into err --
	// otherwise EndField would see a nil error for a field that panicked.
	if obs := st.e.fieldObservers; len(obs) > 0 {
		fi := st.fieldInfo(f, path)
		var inline [observerContexts]context.Context
		begun := inline[:0]
		if len(obs) > len(inline) {
			begun = make([]context.Context, 0, len(obs))
		}
		for _, o := range obs {
			ctx = o.BeginField(ctx, fi)
			begun = append(begun, ctx)
		}
		defer func() {
			for i := len(obs) - 1; i >= 0; i-- {
				obs[i].EndField(begun[i], fi, err)
			}
		}()
	}
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				err = st.recovered(ctx, r, path, f)
			}
		}()
	}
	return f.exec.writeLeaf(ctx, w, parent, args, fc)
}

// callResolve invokes a composite executor with the same protections.
func (st *execState) callResolve(ctx context.Context, f *planField, parent, args any, path *pathNode) (v any, err error) {
	ctx, fc := st.fieldContext(ctx, f, parent, args, path)
	if obs := st.e.fieldObservers; len(obs) > 0 {
		fi := st.fieldInfo(f, path)
		var inline [observerContexts]context.Context
		begun := inline[:0]
		if len(obs) > len(inline) {
			begun = make([]context.Context, 0, len(obs))
		}
		for _, o := range obs {
			ctx = o.BeginField(ctx, fi)
			begun = append(begun, ctx)
		}
		defer func() {
			for i := len(obs) - 1; i >= 0; i-- {
				obs[i].EndField(begun[i], fi, err)
			}
		}()
	}
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				v, err = nil, st.recovered(ctx, r, path, f)
			}
		}()
	}
	return f.exec.resolve(ctx, parent, args, fc)
}

// writeValue writes a composite result: null handling, lists, abstract type
// resolution and objects. path is the full path of the value being written.
func (st *execState) writeValue(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode) bool {
	return st.writeComposite(ctx, w, v, t, shape, f, path, true)
}

// writeComposite is writeValue with an explicit instance check. A list that
// has already decided its elements passes false so each row is not checked
// again, which would turn one batched policy call into N+1.
func (st *execState) writeComposite(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode, checkInstance bool) bool {
	if v == nil || (shape.isNil != nil && shape.isNil(v)) {
		return st.writeNullValue(ctx, w, t, f, path)
	}
	if t.Elem != nil {
		return st.writeList(ctx, w, v, t, shape, f, path)
	}
	obj, v, isNil, err := st.elementObject(f, shape, v)
	if err != nil {
		st.addFieldError(ctx, err, path, f.ast.Position)
		if t.NonNull {
			return false
		}
		w.Null()
		return true
	}
	if isNil {
		return st.writeNullValue(ctx, w, t, f, path)
	}
	if checkInstance {
		o, err := st.instanceOutcome(ctx, f, obj, v)
		if err != nil {
			st.addFieldError(ctx, err, path, f.ast.Position)
			if t.NonNull {
				return false
			}
			w.Null()
			return true
		}
		switch o.act {
		case actionNull:
			return st.writeNullValue(ctx, w, t, f, path)
		case actionDeny:
			st.addFieldError(ctx, o.denial(), path, f.ast.Position)
			if t.NonNull {
				return false
			}
			w.Null()
			return true
		case actionDrop:
			st.addFieldError(ctx, Errorf("authorization: Drop is valid only for a list element, not at %s", coordinate(f.def.object.name, f.def.name)), path, f.ast.Position)
			return st.writeNullValue(ctx, w, t, f, path)
		}
	}
	return st.writeObject(ctx, w, obj, f.sub, v, path, false)
}

// writeNullValue writes null for a nullable position or records the
// non-null violation and returns false.
func (st *execState) writeNullValue(ctx context.Context, w *jsonw.Writer, t *ast.Type, f *planField, path *pathNode) bool {
	if t.NonNull {
		st.addFieldError(ctx, Errorf("Cannot return null for non-nullable field %s.", coordinate(f.def.object.name, f.def.name)), path, f.ast.Position)
		return false
	}
	w.Null()
	return true
}

// concreteValue resolves the object type of an abstract value and normalizes
// the value to *E.
func (s *Schema) concreteValue(at *abstractType, v any) (obj *objectType, ptr any, isNil bool, err error) {
	obj, err = s.concreteType(at, v)
	if err != nil {
		return nil, nil, false, err
	}
	rt := reflect.TypeOf(v)
	switch rt {
	case obj.shapes.ptr:
		if s.reg.nilChecks[rt](v) {
			return obj, nil, true, nil
		}
		return obj, v, false, nil
	case obj.shapes.elem:
		return obj, obj.shapes.toPtr(v), false, nil
	}
	return obj, v, false, nil
}

// elementObject resolves one value to its concrete object type and pointer,
// the work writeValue does between f.target and concreteValue.
func (st *execState) elementObject(f *planField, shape *valueShape, v any) (obj *objectType, ptr any, isNil bool, err error) {
	if v == nil || (shape != nil && shape.isNil != nil && shape.isNil(v)) {
		return nil, nil, true, nil
	}
	if shape != nil && shape.elem != nil {
		// Still a list; the inner writeList decides these values.
		return nil, v, false, nil
	}
	obj = f.target
	if obj != nil {
		if shape != nil && shape.toPtr != nil {
			v = shape.toPtr(v)
		}
		return obj, v, false, nil
	}
	return st.e.schema.concreteValue(f.abstract, v)
}

func instanceSiteOf(f *planField) AuthSite {
	return AuthSite{
		Coord:        f.def.def.Type.Name(),
		Kind:         SiteInstance,
		valueNonNull: positionNonNull(f.def.def.Type),
	}
}

// instanceOutcome asks the ObjectAuthorizer about one value about to be
// written at f. It returns the zero Outcome when nothing guards it, so the
// caller needs no nil check.
func (st *execState) instanceOutcome(ctx context.Context, f *planField, obj *objectType, v any) (Outcome, error) {
	if st.e.objectAuthorizer == nil || !f.hasInstanceSite() || obj == nil || !obj.instanceGuarded {
		return Outcome{}, nil
	}
	site := instanceSiteOf(f)
	outs, err := st.checkObjects(ctx, []ObjectCheck{{Site: site, Type: obj.name, Object: v}})
	if err != nil || len(outs) == 0 {
		return Outcome{}, err
	}
	return outs[0], nil
}

// instanceOutcomes decides a whole drained list at once, which is what keeps
// a remote policy to one call per list rather than one per row. Dropped
// elements leave no null, no error and no gap; keep them out of the write.
//
// Each element is resolved to its concrete type twice, here and again when it
// is written. Carrying the resolved (objectType, pointer) pair forward would
// cost a slice of them per list, which is the worse trade against a path that
// has just made a policy call; on a concrete position the second resolution is
// a nil check and shape.toPtr.
func (st *execState) instanceOutcomes(ctx context.Context, f *planField, shape *valueShape, elems []any) (outs []Outcome, err error) {
	if st.e.objectAuthorizer == nil || !f.hasInstanceSite() || len(elems) == 0 {
		return nil, nil
	}
	site := instanceSiteOf(f)
	elemShape := shape
	if shape != nil && shape.elem != nil {
		elemShape = shape.elem
	}
	checks := make([]ObjectCheck, 0, len(elems))
	at := make([]int, 0, len(elems))
	for i, e := range elems {
		obj, v, isNil, cerr := st.elementObject(f, elemShape, e)
		if cerr != nil || isNil || obj == nil || !obj.instanceGuarded {
			continue
		}
		checks = append(checks, ObjectCheck{Site: site, Type: obj.name, Object: v})
		at = append(at, i)
	}
	if len(checks) == 0 {
		return nil, nil
	}
	batch, err := st.checkObjects(ctx, checks)
	if err != nil {
		return nil, err
	}
	outs = make([]Outcome, len(elems))
	for j, i := range at {
		outs[i] = batch[j]
	}
	return outs, nil
}

func instanceAct(outs []Outcome, i int) action {
	if i < 0 || i >= len(outs) {
		return actionAllow
	}
	return outs[i].act
}

// drainList collects a list's elements. It is a function rather than a closure
// at each call site because a closure appending to a caller's local forces
// that slice header onto the heap on every call of the enclosing function,
// whether or not the list is ever drained -- measured as +1 alloc/op on
// BenchmarkFieldPathBare, which writes no concurrent list at all.
func drainList(v any, shape *valueShape) []any {
	var elems []any
	shape.traverse(v, func(_ int, e any) bool {
		elems = append(elems, e)
		return true
	})
	return elems
}

// writeList writes list elements, nulling failed nullable elements and
// failing the whole list when a non-null element fails. Instance checks
// drain first so one policy call covers the list; without them the sequential
// path stays a single pass, so a lazy source can stop when an element fails
// or the response limit trips.
func (st *execState) writeList(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode) bool {
	if st.e.objectAuthorizer != nil && f.hasInstanceSite() {
		return st.writeListGuarded(ctx, w, v, t, shape, f, path)
	}
	var drained []any
	drainedList := false
	if f.sub != nil && f.sub.deepSchedulable && st.e.sem != nil {
		ok, handled, elems := st.writeListConcurrentPlain(ctx, w, v, t, shape, f, path)
		if handled {
			return ok
		}
		drained, drainedList = elems, true
	}
	mark := w.Mark()
	w.BeginArray()
	failed := false
	writeElem := func(i int, e any) bool {
		em := w.Mark()
		if !st.writeValue(ctx, w, e, t.Elem, shape.elem, f, &pathNode{parent: path, index: i, isIndex: true}) {
			if t.Elem.NonNull || w.LimitExceeded() {
				failed = true
				return false
			}
			w.Rewind(em)
			w.Null()
		}
		return true
	}
	if drainedList {
		for i, e := range drained {
			if !writeElem(i, e) {
				break
			}
		}
	} else {
		shape.traverse(v, writeElem)
	}
	if failed {
		w.Rewind(mark)
		return false
	}
	w.EndArray()
	return true
}

func (st *execState) writeListGuarded(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode) bool {
	drained := drainList(v, shape)
	outs, aerr := st.instanceOutcomes(ctx, f, shape, drained)
	if aerr != nil {
		st.addFieldError(ctx, aerr, path, f.ast.Position)
		return false
	}
	if f.sub != nil && f.sub.deepSchedulable && st.e.sem != nil {
		ok, handled := st.writeListConcurrent(ctx, w, t, shape, f, path, drained, outs)
		if handled {
			return ok
		}
	}
	mark := w.Mark()
	w.BeginArray()
	failed := false
	writeElem := func(i int, e any) bool {
		em := w.Mark()
		if !st.writeComposite(ctx, w, e, t.Elem, shape.elem, f, &pathNode{parent: path, index: i, isIndex: true}, false) {
			if t.Elem.NonNull || w.LimitExceeded() {
				failed = true
				return false
			}
			w.Rewind(em)
			w.Null()
		}
		return true
	}
	n := 0
	for i, e := range drained {
		switch instanceAct(outs, i) {
		case actionDrop:
			continue
		case actionDeny:
			st.addFieldError(ctx, outs[i].denial(), &pathNode{parent: path, index: n, isIndex: true}, f.ast.Position)
			if t.Elem.NonNull {
				failed = true
			} else {
				w.Null()
				n++
			}
		case actionNull:
			if st.writeNullValue(ctx, w, t.Elem, f, &pathNode{parent: path, index: n, isIndex: true}) {
				n++
			} else {
				failed = true
			}
		default:
			if writeElem(n, e) {
				n++
			}
		}
		// Stop where the plain path stops. Once the list has failed it will be
		// rewound, so resolving what follows spends I/O on a response nobody
		// will see and appends one error per element, every one of them
		// reporting the index of the first. The limit check covers the Deny
		// and Null branches, which do not go through writeElem's own.
		if failed || w.LimitExceeded() {
			failed = true
			break
		}
	}
	if failed {
		w.Rewind(mark)
		return false
	}
	w.EndArray()
	return true
}

// taskResult is the outcome of a value written into its own buffer.
type taskResult struct {
	buf *jsonw.Writer
	ok  bool
}

// taskGroup runs closures on the executor's bounded worker budget, falling
// back to inline execution when no slot is free. Panics escaping a
// goroutine (only possible with recovery disabled) are re-raised in the
// waiting goroutine.
type taskGroup struct {
	st       *execState
	async    bool
	wg       sync.WaitGroup
	panicked atomic.Bool
	panicVal any
}

func (g *taskGroup) run(task func()) {
	if g.async {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			if g.st.e.sem != nil {
				select {
				case g.st.e.sem <- struct{}{}:
					defer func() { <-g.st.e.sem }()
				default:
				}
			}
			defer func() {
				if r := recover(); r != nil && g.panicked.CompareAndSwap(false, true) {
					g.panicVal = r
				}
			}()
			task()
		}()
		return
	}
	select {
	case g.st.e.sem <- struct{}{}:
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			defer func() { <-g.st.e.sem }()
			defer func() {
				if r := recover(); r != nil && g.panicked.CompareAndSwap(false, true) {
					g.panicVal = r
				}
			}()
			task()
		}()
	default:
		task()
	}
}

func (g *taskGroup) wait() {
	g.wg.Wait()
	if g.panicked.Load() {
		panic(g.panicVal)
	}
}

// writeFieldsConcurrent executes schedulable fields on the worker budget,
// each into its own buffer, then splices the results in selection order.
// Pure fields are written inline during the splice.
func (st *execState) writeFieldsConcurrent(ctx context.Context, w *jsonw.Writer, obj *objectType, sel *selectionSet, parent any, path *pathNode) bool {
	fields := sel.fields
	results := make([]taskResult, len(fields))
	defer func() {
		for _, r := range results {
			if r.buf != nil {
				jsonw.Put(r.buf)
			}
		}
	}()

	n := 0
	for _, f := range fields {
		if f.schedulable {
			n++
		}
	}
	endWave := st.pushWave(ctx, n)
	defer endWave()

	g := taskGroup{st: st, async: true}
	for i, f := range fields {
		if !f.schedulable {
			continue
		}
		g.run(func() {
			st.waveTaskBegin(ctx)
			defer st.waveTaskEnd(ctx)
			sub := jsonw.Get()
			sub.ShareLimit(w)
			ok := st.writeFieldValue(ctx, sub, obj, f, parent, path)
			results[i] = taskResult{buf: sub, ok: ok}
		})
	}
	g.wait()

	if w.LimitExceeded() {
		return false
	}

	for i, f := range fields {
		if !f.schedulable {
			if !st.writeField(ctx, w, obj, f, parent, path) {
				return false
			}
			continue
		}
		r := results[i]
		switch {
		case r.ok:
			w.Key(f.key)
			w.Splice(r.buf)
		case f.def.typ.NonNull:
			return false
		default:
			w.Key(f.key)
			w.Null()
		}
	}
	return true
}

// writeListConcurrentPlain writes list elements in parallel when there are at
// least two.
//
// It is a deliberate near-copy of writeListConcurrent, which does the same for
// a list whose elements an ObjectAuthorizer has decided. Merging the two by
// giving this one a nil outcomes slice was tried and reverted: the merged loop
// costs an allocation on every concurrent list, guarded or not, and this is
// the hot path (BenchmarkExecuteConcurrentList). The wave, splice and rewind
// halves must stay in step. TestConcurrentListPathsAgree writes one list
// through both and requires the same bytes, which catches a divergence in what
// they write -- a reviewer's reversal of the splice loop in one of them failed
// it -- but not one in a branch that does not change the output for that
// input: breaking only the guarded path's LimitExceeded early return still
// passed. Neither the response limit nor any outcome but Allow is covered by
// it, so a change to those halves needs its own test.
// handled is false when the list is too short; the drained
// elements come back with it so the caller can write them without traversing
// the value again, which a single-pass iter.Seq would answer with nothing.
func (st *execState) writeListConcurrentPlain(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode) (ok, handled bool, drained []any) {
	elems := drainList(v, shape)
	if len(elems) < 2 {
		return false, false, elems
	}
	results := make([]taskResult, len(elems))
	defer func() {
		for _, r := range results {
			if r.buf != nil {
				jsonw.Put(r.buf)
			}
		}
	}()

	endWave := st.pushWave(ctx, len(elems))
	defer endWave()

	g := taskGroup{st: st, async: true}
	// Every element is spawned even after a trip: the wave announced all of
	// them, and a loader flushes only once every announced task has begun, so
	// skipping one would strand each Load already parked. A task started after
	// the trip fails at its first checkpoint.
	for i, e := range elems {
		g.run(func() {
			st.waveTaskBegin(ctx)
			defer st.waveTaskEnd(ctx)
			sub := jsonw.Get()
			sub.ShareLimit(w)
			okElem := st.writeValue(ctx, sub, e, t.Elem, shape.elem, f, &pathNode{parent: path, index: i, isIndex: true})
			results[i] = taskResult{buf: sub, ok: okElem}
		})
	}
	g.wait()

	if w.LimitExceeded() {
		return false, true, nil
	}

	mark := w.Mark()
	w.BeginArray()
	for _, r := range results {
		switch {
		case r.ok:
			w.Splice(r.buf)
		case t.Elem.NonNull:
			w.Rewind(mark)
			return false, true, nil
		default:
			w.Null()
		}
	}
	w.EndArray()
	return true, true, nil
}

// writeListConcurrent writes list elements in parallel when there are at
// least two after drops. See writeListConcurrentPlain for why the two are
// kept apart, and what must stay in step between them. handled is false when the list is too short; the
// caller writes the remaining elements without traversing the value again.
// Drop is removed before pushWave so a loader is not left waiting for a task
// that will never begin; Deny and Null still occupy a written position.
func (st *execState) writeListConcurrent(ctx context.Context, w *jsonw.Writer, t *ast.Type, shape *valueShape, f *planField, path *pathNode, elems []any, outs []Outcome) (ok, handled bool) {
	// keep[j] is the index in elems of the j-th element actually written, so j
	// is its index in the response and Drop renumbers what follows. An index
	// rather than a copied {elem, act, Outcome} per element: Outcome carries a
	// func and two strings, and holding one per element measured +76% B/op
	// here against carrying none.
	keep := make([]int32, 0, len(elems))
	for i := range elems {
		if instanceAct(outs, i) != actionDrop {
			keep = append(keep, int32(i))
		}
	}
	if len(keep) < 2 {
		return false, false
	}
	n := len(keep)

	results := make([]taskResult, n)
	defer func() {
		for _, r := range results {
			if r.buf != nil {
				jsonw.Put(r.buf)
			}
		}
	}()

	endWave := st.pushWave(ctx, n)
	defer endWave()

	g := taskGroup{st: st, async: true}
	// Every remaining element is spawned even after a trip: the wave announced
	// all of them, and a loader flushes only once every announced task has
	// begun, so skipping one would strand each Load already parked. A task
	// started after the trip fails at its first checkpoint.
	for j := range n {
		g.run(func() {
			st.waveTaskBegin(ctx)
			defer st.waveTaskEnd(ctx)
			sub := jsonw.Get()
			sub.ShareLimit(w)
			i := int(keep[j])
			elemPath := &pathNode{parent: path, index: j, isIndex: true}
			var okElem bool
			switch instanceAct(outs, i) {
			case actionDeny:
				st.addFieldError(ctx, outs[i].denial(), elemPath, f.ast.Position)
				if t.Elem.NonNull {
					okElem = false
				} else {
					sub.Null()
					okElem = true
				}
			case actionNull:
				okElem = st.writeNullValue(ctx, sub, t.Elem, f, elemPath)
			default:
				okElem = st.writeComposite(ctx, sub, elems[i], t.Elem, shape.elem, f, elemPath, false)
			}
			results[j] = taskResult{buf: sub, ok: okElem}
		})
	}
	g.wait()

	if w.LimitExceeded() {
		return false, true
	}

	mark := w.Mark()
	w.BeginArray()
	for _, r := range results {
		switch {
		case r.ok:
			w.Splice(r.buf)
		case t.Elem.NonNull:
			w.Rewind(mark)
			return false, true
		default:
			w.Null()
		}
	}
	w.EndArray()
	return true, true
}

func (st *execState) pushWave(ctx context.Context, n int) func() {
	if n <= 0 {
		return func() {}
	}
	oc := OperationFrom(ctx)
	if oc == nil || oc.hub == nil {
		return func() {}
	}
	oc.hub.push(n)
	return oc.hub.pop
}

func (st *execState) waveTaskBegin(ctx context.Context) {
	if oc := OperationFrom(ctx); oc != nil && oc.hub != nil {
		oc.hub.taskBegin()
	}
}

func (st *execState) waveTaskEnd(ctx context.Context) {
	if oc := OperationFrom(ctx); oc != nil && oc.hub != nil {
		oc.hub.taskEnd()
	}
}

// recordCancellation adds a single error describing why execution stopped.
func (st *execState) recordCancellation(ctx context.Context, err error) {
	if st.cancelled.CompareAndSwap(false, true) {
		if st.e.timedOut(ctx) {
			st.addError(ctx, Errorf("%v", st.e.timeoutCause).WithCode(CodeOperationTimeout), nil, nil)
			return
		}
		st.addError(ctx, Errorf("%v", err).WithCode(CodeRequestCancelled), nil, nil)
	}
}
