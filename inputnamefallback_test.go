package graphql

import (
	"context"
	"strings"
	"testing"
)

// An ORM writes snake_case json tags because that is its wire format with the
// database layer, under an SDL that is camelCase because that is GraphQL's.
// Before the fallback these five fields produced ten build errors -- "has no
// binding" for what the schema declares and "is not defined in the schema" for
// what the tag named -- and nothing could be done about it from outside the
// generated code, because Input refuses a second binding for one type name.
type assertionInput struct {
	RoleID         int64  `json:"role_id"`
	OrganizationID int64  `json:"organization_id"`
	EntityID       int64  `json:"entity_id"`
	Expect         string `json:"expect"`
	Note           string `json:"note"`
}

type assertArgs struct{ In assertionInput }

const assertionSDL = `
type Query { ok: Boolean! }
input AssertionInput {
  roleId: Int!
  organizationId: Int!
  entityId: Int!
  expect: String!
  note: String!
}
type Mutation { assert(in: AssertionInput!): String! }
`

func TestInputFallsBackToTheGoFieldName(t *testing.T) {
	s, err := NewSchema(SDL(assertionSDL),
		Query(Field("ok", func(Root) bool { return true })),
		Input[assertionInput]("AssertionInput"),
		Args[assertArgs](InputField("in", func(a *assertArgs, v assertionInput) { a.In = v })),
		Mutation(FieldArgs("assert", func(_ Root, a assertArgs) string { return a.In.Expect })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := NewExecutor(s).Execute(context.Background(), &Request{
		Query: `mutation { assert(in: {roleId: 1, organizationId: 2, entityId: 3, expect: "allow", note: "n"}) }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("execute: %v", resp.Errors[0])
	}
	if got := string(resp.Data); !strings.Contains(got, `"allow"`) {
		t.Fatalf("data = %s", got)
	}
}

// The fallback is a last resort, not a rename: a tag that names a field the
// schema does declare still wins, even when the Go field name would name a
// different declared field. Otherwise two fields could quietly swap.
type swapped struct {
	A string `json:"b"`
	B string `json:"a"`
}

type takeArgs struct{ In swapped }

func TestInputTagWinsOverTheGoFieldName(t *testing.T) {
	s, err := NewSchema(SDL(`
		type Query { ok: Boolean! }
		input Swapped { a: String! b: String! }
		type Mutation { take(in: Swapped!): String! }
	`),
		Query(Field("ok", func(Root) bool { return true })),
		Input[swapped]("Swapped"),
		Args[takeArgs](InputField("in", func(a *takeArgs, v swapped) { a.In = v })),
		Mutation(FieldArgs("take", func(_ Root, x takeArgs) string { return x.In.A + "/" + x.In.B })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := NewExecutor(s).Execute(context.Background(), &Request{
		Query: `mutation { take(in: {a: "1", b: "2"}) }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("execute: %v", resp.Errors[0])
	}
	// a: "1" lands on B (tagged json:"a"), b: "2" on A.
	if got := string(resp.Data); !strings.Contains(got, `"2/1"`) {
		t.Fatalf("data = %s, want the tags to have decided", got)
	}
}
