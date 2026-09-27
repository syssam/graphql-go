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
	e := NewExecutor(rowSchema(t), WithAuthorizer(AuthorizerFunc(
		func(_ context.Context, _ *AuthShape, d *Decision) error { return d.Set(0, RedactRow(nil)) })))
	resp := run(t, e, `{ users { email } }`, "")
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "nil function") {
		t.Fatalf("RedactRow(nil) was not rejected: %s", errorsJSON(resp.Errors))
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
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(func(_ context.Context, _ *AuthShape, d *Decision) error {
		return d.Set(0, RedactRow(func(context.Context, any, any) any { return nil }))
	})))
	resp := run(t, e, `{ me { id } }`, "")
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "leaf field") || strings.Contains(string(resp.Data), `"id"`) {
		t.Fatalf("RedactRow on a composite field was not refused: %s %s", resp.Data, errorsJSON(resp.Errors))
	}
}
