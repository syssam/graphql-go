package graphql

import (
	"context"
	"strings"
	"testing"
)

// The shared instance fixture's only guarded list is `customers: [Customer!]!`,
// so every list test so far has had non-null elements — where a denial takes
// the whole list down and there is nothing to write in place of the refused
// row. The nullable-element shape `[Customer]` is the one an author reaches
// for precisely so that a refused row is a null beside its siblings rather
// than the end of the list, and it was entirely untested: three branches of
// writeListGuarded, including every use of Null() on a list element.

type nlCustomer struct {
	ID   string
	Name string
}

const nullableListSDL = `
directive @authorizeObject on OBJECT

type NLCustomer @authorizeObject { id: ID! name: String! owner: String! }
type Query {
  rows: [NLCustomer]!
  strictRows: [NLCustomer!]!
}
`

var nlRows = []*nlCustomer{{ID: "c1", Name: "one"}, {ID: "c2", Name: "two"}, {ID: "c3", Name: "three"}}

func newNullableListExecutor(t *testing.T, a ObjectAuthorizer) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(nullableListSDL),
		Object[nlCustomer]("NLCustomer",
			Field("id", func(c *nlCustomer) ID { return ID(c.ID) }),
			Field("name", func(c *nlCustomer) string { return c.Name }),
			// A resolver field: selecting it makes the list deeply
			// schedulable, which is what routes writeListGuarded into
			// writeListConcurrent.
			Resolve("owner", func(_ context.Context, c *nlCustomer) (string, error) {
				return "owner-" + c.ID, nil
			}),
		),
		Query(
			Resolve("rows", func(context.Context, Root) ([]*nlCustomer, error) { return nlRows, nil }),
			Resolve("strictRows", func(context.Context, Root) ([]*nlCustomer, error) { return nlRows, nil }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	// The concurrent list path also needs a concurrency budget: without
	// WithMaxConcurrency the executor has no semaphore and writeListGuarded
	// never routes into writeListConcurrent, so a test meaning to compare the
	// two paths would quietly run the serial one twice.
	return NewExecutor(s, WithObjectAuthorizer(a), WithMaxConcurrency(8))
}

// byID answers each check by the id of the object it carries.
func byID(m map[string]Outcome) ObjectAuthorizer {
	return objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		outs := make([]Outcome, len(checks))
		for i, c := range checks {
			if o, ok := m[c.Object.(*nlCustomer).ID]; ok {
				outs[i] = o
			}
		}
		return outs, nil
	})
}

// A denial of one row must leave the others alone, and must leave a null where
// the row would have been so the client's indices still line up with what it
// asked for.
func TestInstanceDenyOnANullableListElementNullsOnlyThatElement(t *testing.T) {
	e := newNullableListExecutor(t, byID(map[string]Outcome{
		"c2": Deny("customer:read", "NLCustomer"),
	}))
	resp := run(t, e, `{ rows { id } }`, "")
	if got, want := string(resp.Data), `{"rows":[{"id":"c1"},null,{"id":"c3"}]}`; got != want {
		t.Fatalf("data = %s, want %s -- a denied row must be a null in place, not the "+
			"end of the list and not a silently shortened one", got, want)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %s, want exactly one for the denied row", errorsJSON(resp.Errors))
	}
	if got := resp.Errors[0].Path.String(); got != "rows[1]" {
		t.Errorf("path = %q, want rows[1]: the error must name the index the null was "+
			"written at, or a client cannot tell which row was refused", got)
	}
}

// Null() is a documented Outcome and had never been applied to a list element.
// It differs from Deny in exactly one way that matters here: no error.
func TestInstanceNullOnANullableListElement(t *testing.T) {
	e := newNullableListExecutor(t, byID(map[string]Outcome{"c1": Null(), "c3": Null()}))
	resp := run(t, e, `{ rows { id } }`, "")
	if got, want := string(resp.Data), `{"rows":[null,{"id":"c2"},null]}`; got != want {
		t.Fatalf("data = %s, want %s", got, want)
	}
	if len(resp.Errors) != 0 {
		t.Errorf("Null() reported %s; it is the outcome that hides a row without "+
			"saying so, which is the whole difference from Deny", errorsJSON(resp.Errors))
	}
}

// And the contrast that explains why the nullable shape exists: with non-null
// elements there is nowhere to put the null, so one refused row takes the
// whole list with it.
func TestInstanceNullOnANonNullListElementTakesTheList(t *testing.T) {
	e := newNullableListExecutor(t, byID(map[string]Outcome{"c2": Null()}))
	resp := run(t, e, `{ strictRows { id } }`, "")
	if got := string(resp.Data); got != `null` {
		t.Fatalf("data = %s, want null: a null element of [NLCustomer!]! must bubble", got)
	}
	if len(resp.Errors) == 0 {
		t.Error("the bubbled null produced no error")
	}
	if body := errorsJSON(resp.Errors); !strings.Contains(body, "strictRows") {
		t.Errorf("error does not name the field: %s", body)
	}
}

// writeListConcurrent re-implements every one of these outcomes for the
// scheduled path, and which path a list takes is decided by whether the
// selection happens to contain a resolver field. A divergence between the two
// would mean the same policy on the same data authorizes differently depending
// on which fields the client asked for -- so the two are asserted against each
// other rather than separately.
//
// Every branch below was uncovered: deny and Null on a nullable element, and
// the non-null rewind, all on the concurrent side.
func TestGuardedListAuthorizesTheSameOnBothPaths(t *testing.T) {
	for _, c := range []struct {
		name     string
		policy   map[string]Outcome
		field    string
		serial   string
		wantErrs int
	}{
		{
			name:     "deny a middle row",
			policy:   map[string]Outcome{"c2": Deny("customer:read", "NLCustomer")},
			field:    "rows",
			serial:   `{"rows":[{"id":"c1"},null,{"id":"c3"}]}`,
			wantErrs: 1,
		},
		{
			name:   "null the outer rows",
			policy: map[string]Outcome{"c1": Null(), "c3": Null()},
			field:  "rows",
			serial: `{"rows":[null,{"id":"c2"},null]}`,
		},
		{
			name:     "deny inside a non-null list bubbles",
			policy:   map[string]Outcome{"c2": Deny("customer:read", "NLCustomer")},
			field:    "strictRows",
			serial:   `null`,
			wantErrs: 1,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Serial: only pure fields selected.
			serial := run(t, newNullableListExecutor(t, byID(c.policy)),
				"{ "+c.field+" { id } }", "")
			if got := string(serial.Data); got != c.serial {
				t.Fatalf("serial data = %s, want %s", got, c.serial)
			}

			// Concurrent: the same query plus the resolver field, which is the
			// only difference. Compare the shape, with owner removed, against
			// the serial answer.
			concurrent := run(t, newNullableListExecutor(t, byID(c.policy)),
				"{ "+c.field+" { id owner } }", "")
			stripped := strings.ReplaceAll(string(concurrent.Data), `,"owner":"owner-c1"`, "")
			stripped = strings.ReplaceAll(stripped, `,"owner":"owner-c2"`, "")
			stripped = strings.ReplaceAll(stripped, `,"owner":"owner-c3"`, "")
			if stripped != c.serial {
				t.Errorf("the concurrent path authorized differently\n concurrent: %s\n     serial: %s\n"+
					"the same policy on the same rows must not depend on whether the "+
					"selection contains a resolver field", stripped, c.serial)
			}
			if len(serial.Errors) != c.wantErrs || len(concurrent.Errors) != c.wantErrs {
				t.Errorf("errors: serial %d, concurrent %d, want %d each\n serial: %s\n conc: %s",
					len(serial.Errors), len(concurrent.Errors), c.wantErrs,
					errorsJSON(serial.Errors), errorsJSON(concurrent.Errors))
			}
		})
	}
}
