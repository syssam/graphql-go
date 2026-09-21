package graphql

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// checkObjects asks the ObjectAuthorizer about one wave's worth of resolved
// values, splitting the call at the configured batch size. The result is
// positional: outs[i] is the outcome for checks[i].
//
// A batch that fails -- an error, a mismatched length, or an outcome the
// site cannot represent -- fails every check still outstanding rather than
// allowing the checks a later batch would have covered: a policy backend
// that is down or misbehaving must not be the reason a row becomes visible.
func (st *execState) checkObjects(ctx context.Context, checks []ObjectCheck) (outs []Outcome, err error) {
	a := st.e.objectAuthorizer
	if a == nil || len(checks) == 0 {
		return nil, nil
	}
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "graphql: object authorizer panic",
					"panic", r,
					"stack", string(debug.Stack()),
				)
				outs, err = nil, authorizerError(ctx, &panicError{value: r})
			}
		}()
	}
	outs = make([]Outcome, 0, len(checks))
	for start := 0; start < len(checks); start += st.e.objectAuthBatch {
		end := min(start+st.e.objectAuthBatch, len(checks))
		// Three-index: the batch must not carry capacity into the checks the
		// next call has not been asked about, or a policy that appends to the
		// slice it was handed rewrites them before they are sent.
		batch, berr := a.AuthorizeObjects(ctx, checks[start:end:end])
		if berr != nil {
			return nil, authorizerError(ctx, berr)
		}
		if len(batch) != end-start {
			return nil, authorizerError(ctx, Errorf(
				"authorization: ObjectAuthorizer returned %d outcomes for %d checks at %s",
				len(batch), end-start, checks[start].Site.Coord))
		}
		// Against each check's own site, not one site for the batch: a
		// coalesced batch carries checks from several positions, and Null is
		// valid at a nullable one and not at a non-null one.
		for i, o := range batch {
			if verr := o.validFor(checks[start+i].Site); verr != nil {
				return nil, authorizerError(ctx, verr)
			}
		}
		outs = append(outs, batch...)
	}
	return outs, nil
}
