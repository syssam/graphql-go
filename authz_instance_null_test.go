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

// checkObjects hands the policy a window into its own checks slice. A
// three-index slice keeps that window's capacity at its length, so a policy
// that appends to the batch it was given allocates instead of overwriting the
// checks the next batch has not been asked about yet.
func TestCheckObjectsBatchHasNoSpareCapacity(t *testing.T) {
	var widest int
	e := newInstanceExecutorWith(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		if c := cap(checks) - len(checks); c > widest {
			widest = c
		}
		return make([]Outcome, len(checks)), nil
	}), WithObjectAuthBatch(2))
	run(t, e, `{ customers { id } }`, "")
	if widest != 0 {
		t.Fatalf("a batch carried %d elements of spare capacity; an append by the policy would reach the next batch's checks", widest)
	}
}

// A non-null element that is denied fails the whole list, which is then
// rewound. Continuing to decide and resolve the elements after it spends I/O
// on a response nobody will see and appends an error per element, all of them
// reporting the index of the first -- the plain path stops at the first
// failure for exactly this reason.
func TestInstanceDenyStopsANonNullList(t *testing.T) {
	e := newInstanceExecutorWith(t, constantObjectPolicy(Deny("read", "Customer")))
	resp := run(t, e, `{ customers { id } }`, "")
	assertJSON(t, resp.Data, `null`)
	if len(resp.Errors) != 1 {
		t.Fatalf("%d errors, want 1: the list kept going after it had already failed", len(resp.Errors))
	}
	if got := resp.Errors[0].Path.String(); got != "customers[0]" {
		t.Fatalf("path = %q, want customers[0]", got)
	}
}

// writeListConcurrentPlain and writeListConcurrent are deliberate near-copies
// (merging them costs an allocation on every concurrent list). Nothing else
// notices if one is changed and the other is not, so write the same list
// through both -- the plain path with no ObjectAuthorizer configured, the
// guarded path with one that allows everything -- and require the same bytes.
//
// What it holds: a divergence in what the two loops write. Reversing the
// splice loop in one of them fails it. What it does not hold: a branch that
// does not change the output for this input -- breaking only the guarded
// path's LimitExceeded early return passed -- so the limit subtest pins that
// the two agree once tripped, not that either check exists.
func TestConcurrentListPathsAgree(t *testing.T) {
	const q = `{ customers { id name owner } }`
	for _, c := range []struct {
		name string
		opts []ExecutorOption
	}{
		{name: "success"},
		{name: "response limit tripped", opts: []ExecutorOption{WithMaxResponseBytes(64)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain := run(t, newInstanceExecutor(t, c.opts...), q, "")
			guarded := run(t, newInstanceExecutorWith(t, constantObjectPolicy(Allow()), c.opts...), q, "")
			if string(plain.Data) != string(guarded.Data) {
				t.Fatalf("the two concurrent list paths diverged:\n plain   = %s\n guarded = %s", plain.Data, guarded.Data)
			}
			if len(plain.Errors) != len(guarded.Errors) {
				t.Fatalf("errors diverged: plain = %v, guarded = %v", plain.Errors, guarded.Errors)
			}
		})
	}
}
