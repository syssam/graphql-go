package graphql

import (
	"context"
	"strings"
	"testing"
)

// docs/spec-conformance.md lists what was "probed directly, not assumed" --
// but probing is what someone did once, at audit time, with a throwaway
// program. Two of those claims had no test, so the document asserts behaviour
// that nothing keeps true. A published conformance claim that regresses
// silently is worse than no claim.

type specIDArgs struct{ ID ID }

// "Scalar input coercion: ... ID accepting Int but not Float."
//
// Both halves matter. Accepting Int is what lets a client send a database
// bigint unquoted. Refusing Float is what stops 1.5 -- and 5.0, which is
// integral and still not an Int -- becoming an id silently, which would be a
// lookup for a row that does not exist or, worse, a different one.
func TestSpecIDAcceptsIntAndStringOnly(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { row(id: ID!): String! }`),
		Args[specIDArgs](InputField("id", func(a *specIDArgs, v ID) { a.ID = v })),
		Query(ResolveArgs("row", func(_ context.Context, _ Root, a specIDArgs) (string, error) {
			return string(a.ID), nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)

	for _, c := range []struct {
		name, literal, varJSON, want string
		ok                           bool
	}{
		{name: "int", literal: `5`, varJSON: `5`, want: `{"row":"5"}`, ok: true},
		{name: "negative int", literal: `-5`, varJSON: `-5`, want: `{"row":"-5"}`, ok: true},
		{name: "string", literal: `"abc"`, varJSON: `"abc"`, want: `{"row":"abc"}`, ok: true},
		{name: "float", literal: `1.5`, varJSON: `1.5`},
		// Integral but still a Float: the specification's ID accepts Int or
		// String, and "looks whole" is not one of them.
		{name: "whole float", literal: `5.0`, varJSON: `5.0`},
		{name: "boolean", literal: `true`, varJSON: `true`},
	} {
		t.Run(c.name, func(t *testing.T) {
			lit := run(t, e, `{ row(id: `+c.literal+`) }`, "")
			vr := run(t, e, `query($id: ID!){ row(id: $id) }`, `{"id":`+c.varJSON+`}`)

			for _, got := range []struct {
				form string
				resp *Response
			}{{"literal", lit}, {"variable", vr}} {
				if c.ok {
					if len(got.resp.Errors) > 0 {
						t.Errorf("%s %s was refused: %s", got.form, c.literal,
							errorsJSON(got.resp.Errors))
						continue
					}
					if s := string(got.resp.Data); s != c.want {
						t.Errorf("%s %s = %s, want %s", got.form, c.literal, s, c.want)
					}
					continue
				}
				if len(got.resp.Errors) == 0 {
					t.Errorf("%s %s was accepted as an ID: %s", got.form, c.literal, got.resp.Data)
					continue
				}
				if !strings.Contains(got.resp.Errors[0].Message, "ID") {
					t.Errorf("%s %s: error does not name ID: %s", got.form, c.literal,
						got.resp.Errors[0].Message)
				}
			}
		})
	}
}

// "Response shape: ... data absent on a request error, data: null on a root
// field error."
//
// This is section 7.1 and it is the difference a client keys on: a request
// error means the operation never ran, so there is no data member at all; a
// field error means it ran and produced null. Emitting `"data":null` for a
// validation failure tells the client the query executed and returned nothing,
// which is a different fact.
func TestSpecDataIsAbsentOnARequestErrorAndNullOnAFieldError(t *testing.T) {
	_, e := newFixtureExecutor(t)

	for _, c := range []struct{ name, query string }{
		{"validation failure", `{ nope }`},
		{"parse failure", `{`},
		{"variable coercion failure", `query($v: Int!){ echo(v: $v) }`},
	} {
		t.Run("request error: "+c.name, func(t *testing.T) {
			resp := run(t, e, c.query, "")
			if len(resp.Errors) == 0 {
				t.Fatalf("no error for %q", c.query)
			}
			if resp.Data != nil {
				t.Errorf("data is present (%s) for a request error; section 7.1 says the "+
					"data member must not be present when the operation never ran", resp.Data)
			}
			if !resp.HasRequestErrors() {
				t.Error("HasRequestErrors is false for a request error")
			}
			if body := string(mustJSON(t, resp)); strings.Contains(body, `"data"`) {
				t.Errorf("the serialized envelope carries a data member: %s", body)
			}
		})
	}

	// fail is a non-null String! whose resolver errors, so the field error
	// bubbles to the root and data is the JSON null -- present, and null.
	t.Run("field error on a non-null root field", func(t *testing.T) {
		resp := run(t, e, `{ fail }`, "")
		if len(resp.Errors) == 0 {
			t.Fatal("no error")
		}
		if got := string(resp.Data); got != "null" {
			t.Errorf("data = %q, want the JSON null: the operation ran", got)
		}
		if resp.HasRequestErrors() {
			t.Error("HasRequestErrors is true for a field error")
		}
		if body := string(mustJSON(t, resp)); !strings.Contains(body, `"data":null`) {
			t.Errorf("the serialized envelope has no data:null member: %s", body)
		}
	})
}

func mustJSON(t *testing.T, r *Response) []byte {
	t.Helper()
	b, err := r.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	return b
}
