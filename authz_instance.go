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
func (st *execState) checkObjects(ctx context.Context, site AuthSite, checks []ObjectCheck) (outs []Outcome, err error) {
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
		batch, berr := a.AuthorizeObjects(ctx, checks[start:end])
		if berr != nil {
			return nil, authorizerError(ctx, berr)
		}
		if len(batch) != end-start {
			return nil, authorizerError(ctx, Errorf(
				"authorization: ObjectAuthorizer returned %d outcomes for %d checks at %s",
				len(batch), end-start, site.Coord))
		}
		for _, o := range batch {
			if verr := o.validFor(site); verr != nil {
				return nil, authorizerError(ctx, verr)
			}
		}
		outs = append(outs, batch...)
	}
	return outs, nil
}
