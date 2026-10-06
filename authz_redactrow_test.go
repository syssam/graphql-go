package graphql

import (
	"context"
	"strings"
	"testing"
)

type rowUser struct{ ID, Email string }

type viewerKey struct{}

const rowSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type User { id: ID! email: String! @requiresScopes(scopes: [["pii"]]) }
type Query { users: [User!]! }
`

func rowSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(rowSDL),
		Query(Resolve("users", func(context.Context, Root) ([]*rowUser, error) {
			return []*rowUser{{"1", "a@example.com"}, {"2", "b@example.com"}, {"3", "c@example.com"}}, nil
		})),
		Object[rowUser]("User",
			Field("id", func(u *rowUser) ID { return ID(u.ID) }),
			Field("email", func(u *rowUser) string { return u.Email }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A customer sees their own address and everyone else's masked: the rule is
// about the row, which a Decision is made before. RedactRow is handed the
// row and the field's context, per row, and what it returns is written.
func TestRedactRowDecidesPerRow(t *testing.T) {
	ownOnly := RedactRow(func(ctx context.Context, parent, value any) any {
		if u, ok := parent.(*rowUser); ok && ctx.Value(viewerKey{}) == u.ID {
			return value
		}
		return "***"
	})
	e := NewExecutor(rowSchema(t), WithAuthorizer(AuthorizerFunc(
		func(_ context.Context, _ *AuthShape, d *Decision) error { return d.Set(0, ownOnly) })))

	ctx := context.WithValue(context.Background(), viewerKey{}, "2")
	resp := e.Execute(ctx, &Request{Query: `{ users { id email } }`})
	expectData(t, resp, `{"users":[{"id":"1","email":"***"},{"id":"2","email":"b@example.com"},{"id":"3","email":"***"}]}`)

	// Nobody signed in: every row masked, none failing open.
	resp = e.Execute(context.Background(), &Request{Query: `{ users { email } }`})
	expectData(t, resp, `{"users":[{"email":"***"},{"email":"***"},{"email":"***"}]}`)
}

func TestRedactRowNilFunctionRejected(t *testing.T) {
	logged := captureLog(t)
	e := NewExecutor(rowSchema(t), WithAuthorizer(AuthorizerFunc(
		func(_ context.Context, _ *AuthShape, d *Decision) error { return d.Set(0, RedactRow(nil)) })))
	resp := run(t, e, `{ users { email } }`, "")
	assertPolicyMistake(t, resp.Errors, logged, "nil function")
}

// A list of leaves is a leaf to the schema builder and not to Redact. The
// function is written for one value -- mask this address -- and handing it
// the whole slice instead would be the same call meaning something else: a
// policy that type-asserts a string returns its argument untouched when the
// assertion fails, which serves the list unredacted.
func TestRedactIsRefusedOnAListOfLeaves(t *testing.T) {
	s, err := NewSchema(SDL(`
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { emails: [String!]! @requiresScopes(scopes: [["x"]]) }
`),
		Query(Field("emails", func(Root) []string { return []string{"ada@example.com"} })),
	)
	if err != nil {
		t.Fatal(err)
	}
	mask := func(v any) any {
		if s, ok := v.(string); ok {
			return "***" + s[len(s)-4:]
		}
		return v
	}
	for name, o := range map[string]Outcome{
		"Redact":    Redact(mask),
		"RedactRow": RedactRow(func(_ context.Context, _, v any) any { return mask(v) }),
	} {
		t.Run(name, func(t *testing.T) {
			logged := captureLog(t)
			e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
				func(_ context.Context, _ *AuthShape, d *Decision) error { return d.Set(0, o) })))
			resp := run(t, e, `{ emails }`, "")
			if strings.Contains(string(resp.Data), "ada@") {
				t.Fatalf("the list was served unredacted: %s", resp.Data)
			}
			assertPolicyMistake(t, resp.Errors, logged, "Redact is valid only on a leaf field, not Query.emails")
		})
	}
}

// On a composite field there is no value to rewrite, and the composite path
// never consults a redaction: accepting one there would serve the field
// unguarded.
func TestRedactRowOnlyOnLeafFields(t *testing.T) {
	s, err := NewSchema(SDL(`
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type User { id: ID! }
type Query { me: User @requiresScopes(scopes: [["x"]]) }
`),
		Query(Resolve("me", func(context.Context, Root) (*rowUser, error) { return &rowUser{ID: "1"}, nil })),
		Object[rowUser]("User", Field("id", func(u *rowUser) ID { return ID(u.ID) })),
	)
	if err != nil {
		t.Fatal(err)
	}
	logged := captureLog(t)
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(func(_ context.Context, _ *AuthShape, d *Decision) error {
		return d.Set(0, RedactRow(func(context.Context, any, any) any { return nil }))
	})))
	resp := run(t, e, `{ me { id } }`, "")
	if strings.Contains(string(resp.Data), `"id"`) {
		t.Fatalf("RedactRow on a composite field was not refused: %s %s", resp.Data, errorsJSON(resp.Errors))
	}
	assertPolicyMistake(t, resp.Errors, logged, "leaf field")
}
