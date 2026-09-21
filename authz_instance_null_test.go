package graphql

import (
	"context"
	"strings"
	"testing"
)

// Null at a non-null position is rejected for every other site kind, because
// enforceAuth would otherwise write a spec-violating null with no error. An
// instance site leaves AuthSite.Field nil, so the guard that does this needs
// the position's nullability recorded on the site itself.
func TestInstanceNullIsRejectedAtANonNullField(t *testing.T) {
	e := newInstanceExecutorWith(t, constantObjectPolicy(Null()))
	resp := run(t, e, `{ required { id } }`, "")
	assertJSON(t, resp.Data, `null`)
	assertErrorContains(t, resp.Errors, "instance site")
	assertErrorContains(t, resp.Errors, "Customer")
}

func TestInstanceNullIsRejectedAtANonNullListElement(t *testing.T) {
	// customers is [Customer!]!: an element may not be null, so Null is a
	// policy mistake there and Drop is the outcome that means "not visible".
	e := newInstanceExecutorWith(t, constantObjectPolicy(Null()))
	resp := run(t, e, `{ customers { id } }`, "")
	assertJSON(t, resp.Data, `null`)
	assertErrorContains(t, resp.Errors, "instance site")
}

func TestInstanceNullIsAllowedAtANullableField(t *testing.T) {
	e := newInstanceExecutorWith(t, constantObjectPolicy(Null()))
	resp := run(t, e, `{ maybe { id } }`, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors = %v, want none", resp.Errors)
	}
	assertJSON(t, resp.Data, `{"maybe":null}`)
}

// An instance site is published in AuthShape.Sites() so an Authorizer can see
// that instance checks will happen, but its outcome comes from the
// ObjectAuthorizer and the executor never reads it back. Accepting a Deny
// here and discarding it is the silent allow this branch rejects everywhere
// else, so Set must say so.
func TestDecisionSetRejectsAnInstanceSite(t *testing.T) {
	var setErr error
	var saw bool
	e := newInstanceExecutor(t, WithAuthorizer(AuthorizerFunc(func(_ context.Context, sh *AuthShape, d *Decision) error {
		for i, s := range sh.Sites() {
			if s.Kind == SiteInstance {
				saw = true
				setErr = d.Set(i, Deny("read", "Customer"))
			}
		}
		return nil
	})))
	run(t, e, `{ customers { id } }`, "")
	if !saw {
		t.Fatal("no instance site was published to the Authorizer")
	}
	if setErr == nil {
		t.Fatal("Decision.Set accepted an outcome for an instance site and discarded it")
	}
	if !strings.Contains(setErr.Error(), "ObjectAuthorizer") {
		t.Fatalf("error does not point at the ObjectAuthorizer: %v", setErr)
	}
	if !strings.Contains(setErr.Error(), "Customer") {
		t.Fatalf("error does not name the coordinate: %v", setErr)
	}
}
