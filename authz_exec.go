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
func (st *execState) enforceAuth(ctx context.Context, w *jsonw.Writer, f *planField, path *pathNode) (done, ok bool) {
	o := st.decision.Outcome(int(f.authIdx))
	switch o.act {
	case actionDeny:
		st.fieldError(ctx, o.denial(), path, f)
		return true, false
	case actionNull:
		w.Null()
		return true, true
	case actionZero:
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
// rather than fd.anyResolve on the shared fieldDef. The two differ in two
// cases this field must not skip: a registered FieldInterceptor wraps only
// pf.exec (interceptedExec), and a subscription's per-event root field
// substitutes only pf.exec (runSubscriptionEvent) -- fd.anyResolve on that
// field is still the errSubscriptionResolved stub, since the event never
// touches the shared fieldDef.
func (st *execState) callLeafRedacted(ctx context.Context, w *jsonw.Writer, f *planField, parent, args any, path *pathNode, fn func(any) any) (err error) {
	fd := f.def
	ctx = st.fieldContext(ctx, f, parent, args, path)
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				err = st.recovered(ctx, r, path, f)
			}
		}()
	}
	v, rerr := f.exec.resolveAny(ctx, parent, args)
	if rerr != nil {
		return rerr
	}
	return fd.writeAny(w, fn(v), fd.typ)
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
