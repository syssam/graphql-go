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
	if v == nil || (shape.isNil != nil && shape.isNil(v)) {
		return st.writeNullValue(ctx, w, t, f, path)
	}
	if t.Elem != nil {
		return st.writeList(ctx, w, v, t, shape, f, path)
	}
	obj := f.target
	if obj != nil {
		if shape.toPtr != nil {
			v = shape.toPtr(v)
		}
	} else {
		var err error
		var isNil bool
		obj, v, isNil, err = st.e.schema.concreteValue(f.abstract, v)
		if err != nil {
			st.addError(ctx, err, path.materialize(), f.ast.Position)
			if t.NonNull {
				return false
			}
			w.Null()
			return true
		}
		if isNil {
			return st.writeNullValue(ctx, w, t, f, path)
		}
	}
	return st.writeObject(ctx, w, obj, f.sub, v, path, false)
}

// writeNullValue writes null for a nullable position or records the
// non-null violation and returns false.
func (st *execState) writeNullValue(ctx context.Context, w *jsonw.Writer, t *ast.Type, f *planField, path *pathNode) bool {
	if t.NonNull {
		st.addError(ctx, Errorf("Cannot return null for non-nullable field %s.", coordinate(f.def.object.name, f.def.name)), path.materialize(), f.ast.Position)
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

// writeList writes list elements, nulling failed nullable elements and
// failing the whole list when a non-null element fails.
func (st *execState) writeList(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode) bool {
	var drained []any
	drainedList := false
	if f.sub != nil && f.sub.deepSchedulable && st.e.sem != nil {
		ok, handled, elems := st.writeListConcurrent(ctx, w, v, t, shape, f, path)
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

// writeListConcurrent writes list elements in parallel when there are at
// least two. handled is false when the list is too short; the drained
// elements come back with it so the caller can write them without traversing
// the value again, which a single-pass iter.Seq would answer with nothing.
func (st *execState) writeListConcurrent(ctx context.Context, w *jsonw.Writer, v any, t *ast.Type, shape *valueShape, f *planField, path *pathNode) (ok, handled bool, drained []any) {
	var elems []any
	shape.traverse(v, func(_ int, e any) bool {
		elems = append(elems, e)
		return true
	})
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
		st.addError(ctx, Errorf("%v", err).WithCode(CodeRequestCancelled), nil, nil)
	}
}
