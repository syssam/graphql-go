package graphql

import (
	"context"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// enforceAuth applies the decision recorded for f. done reports whether it
// wrote the field itself, in which case ok is writeFieldValue's result.
//
// Redact is the only outcome that still runs the resolver, so it is the only
// one not decided here; it is handled at the leaf call in exec_object.go.
func (st *execState) enforceAuth(ctx context.Context, w *jsonw.Writer, f *planField, path *pathNode, ord int32) (done, ok bool) {
	// A denied argument refuses the field whatever its output outcome is:
	// checked first because Redact would otherwise run the resolver with the
	// input the policy refused, and Null or Zero would hide that the request
	// was refused at all. The argument sites follow the output site
	// contiguously (see planField.argSites).
	for i := f.authIdx + 1; i <= f.authIdx+f.argSiteCount(); i++ {
		if o := st.decision.Outcome(int(i)); o.act == actionDeny {
			st.fieldError(ctx, o.denial(), path, f, ord)
			return true, false
		}
	}
	o := st.decision.Outcome(int(f.authIdx))
	switch o.act {
	case actionDeny:
		st.fieldError(ctx, o.denial(), path, f, ord)
		return true, false
	case actionNull:
		w.Null()
		return true, true
	case actionZero:
		// f.def is nil only for __typename's SiteObject, and validFor already
		// rejects Zero there (Field == nil) before Decision.Set can store it --
		// guarded anyway since this dispatches on stored state, not a call this
		// function controls, and the check costs nothing next to the write below.
		if f.def == nil {
			return false, true
		}
		writeZero(w, f.def.typ)
		return true, true
	}
	return false, true
}

// authOutcome is the zero Outcome when no decision covers f, so the caller
// needs no nil check.
func (st *execState) authOutcome(f *planField) Outcome {
	if st.decision == nil || f.authIdx < 0 {
		return Outcome{}
	}
	return st.decision.Outcome(int(f.authIdx))
}

// callLeafRedacted mirrors callLeaf, rewriting the resolved value before it
// is written. It repeats callLeaf's panic guard rather than wrapping it
// because the value has to be intercepted between resolve and write, and
// callLeaf does both.
//
// It resolves through f.exec.resolveAny, the plan field's own executor,
// rather than fd.anyResolve on the shared fieldDef, whenever the two differ:
// a registered FieldInterceptor wraps only pf.exec (interceptedExec), and a
// subscription's per-event root field substitutes only pf.exec
// (runSubscriptionEvent) -- fd.anyResolve on that field is still the
// errSubscriptionResolved stub, since the event never touches the shared
// fieldDef. Where they do not differ, resolveAny is nil and fd.anyResolve is
// exactly the executor.
//
// It also mirrors callLeaf's FieldObserver handling, including the order of
// the two defers. A redacted field is still a field that was resolved, and an
// observer -- ext/otel's field spans among them -- that did not see it would
// report a query as having done less than it did.
func (st *execState) callLeafRedacted(ctx context.Context, w *jsonw.Writer, f *planField, parent, args any, path *pathNode, o Outcome) (err error) {
	fd := f.def
	ctx, fc := st.fieldContext(ctx, f, parent, args, path)
	// As in callLeaf: EndField's defer is registered before the recovery
	// defer so it runs after recovery has turned a panic into err.
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
	var v any
	var rerr error
	if f.exec.resolveAny != nil {
		v, rerr = f.exec.resolveAny(ctx, parent, args, fc)
	} else {
		v, rerr = fd.anyResolve(ctx, parent, args)
	}
	if rerr != nil {
		return rerr
	}
	if o.act == actionRedactRow {
		return fd.writeAny(w, o.redact(&redactRowArgs{ctx, parent, v}), fd.typ)
	}
	return fd.writeAny(w, o.redact(v), fd.typ)
}

// writeZero writes the zero value of t. validFor has already refused any type
// this cannot spell, so the unhandled default is unreachable rather than a
// silent wrong answer.
func writeZero(w *jsonw.Writer, t *ast.Type) {
	if t.Elem != nil {
		w.BeginArray()
		w.EndArray()
		return
	}
	switch t.NamedType {
	case "String", "ID":
		w.String("")
	case "Int":
		w.Int64(0)
	case "Float":
		_ = w.Float64(0)
	case "Boolean":
		w.Bool(false)
	}
}
