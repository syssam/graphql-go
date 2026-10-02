package graphql

import (
	"context"
	"testing"
)

type nnSecret struct{ V string }

type nnBox struct{}

// An ObjectAuthorizer has three ways to withhold a row and each is valid at
// only some positions: Drop in a list, Null where the position is nullable,
// Deny anywhere. The site said whether it was a list element and not whether
// it was nullable, so the policy below -- the one that gives nothing away --
// could not be written: Null at a non-null position is a policy error that
// costs the whole request.
func TestAnObjectPolicyCanChooseNullFromTheSiteAlone(t *testing.T) {
	s, err := NewSchema(SDL(`
		directive @authorizeObject on OBJECT
		type Secret @authorizeObject { v: String! }
		type Box { nullable: Secret required: Secret! list: [Secret!]! }
		type Query { box: Box }
	`),
		Object[nnSecret]("Secret", Field("v", func(s *nnSecret) string { return s.V })),
		Object[nnBox]("Box",
			Field("nullable", func(*nnBox) *nnSecret { return &nnSecret{V: "x"} }),
			Field("required", func(*nnBox) *nnSecret { return &nnSecret{V: "x"} }),
			Field("list", func(*nnBox) []*nnSecret { return []*nnSecret{{V: "x"}} }),
		),
		Query(Field("box", func(Root) *nnBox { return &nnBox{} })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	policy := objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		out := make([]Outcome, len(checks))
		for i, c := range checks {
			switch {
			case c.Site.ListElement:
				out[i] = Drop()
			case c.Site.NonNull:
				out[i] = Deny("secret:read", c.Type)
			default:
				out[i] = Null()
			}
		}
		return out, nil
	})
	e := NewExecutor(s, WithObjectAuthorizer(policy))

	// Nullable and list positions give nothing away: no error, no row.
	expectData(t, run(t, e, `{ box { nullable { v } list { v } } }`, ""), `{"box":{"nullable":null,"list":[]}}`)

	// The non-null one has to refuse, and does so as a denial rather than as
	// the policy's own mistake.
	resp := run(t, e, `{ box { required { v } } }`, "")
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != CodeForbidden {
		t.Fatalf("errors = %s, want one FORBIDDEN", errorsJSON(resp.Errors))
	}
}
