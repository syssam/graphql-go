package graphql

import "testing"

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
