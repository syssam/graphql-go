package graphql

import (
	"context"
	"testing"
)

// Code that plans loads from a selection -- an ORM eager-loading edges --
// must be able to skip a subtree authorization will not resolve, or it
// queries rows the caller may not see and nothing will write. Withheld is
// true for exactly the outcomes that never resolve the field.
func TestSelectedFieldWithheld(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { me: User }
type User {
  name: String!
  orders: [Order!] @requiresScopes(scopes: [["orders"]])
  salary: Int @requiresScopes(scopes: [["hr"]])
  email: String @requiresScopes(scopes: [["pii"]])
  nick: String @requiresScopes(scopes: [["public"]])
}
type Order { id: ID! }`
	type user struct{}
	s, err := NewSchema(SDL(sdl),
		Query(Resolve("me", func(context.Context, Root) (*user, error) { return &user{}, nil })),
		Object[user]("User",
			Field("name", func(*user) string { return "n" }),
			Field("orders", func(*user) []struct{} { return nil }),
			Field("salary", func(*user) *int { return nil }),
			Field("email", func(*user) *string { v := "a@b.c"; return &v }),
			Field("nick", func(*user) *string { return nil }),
		),
		Object[struct{}]("Order", Field("id", func(*struct{}) ID { return "1" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]Outcome{
		"User.orders": Null(),
		"User.salary": Deny("hr", "salary"),
		"User.email":  Redact(func(any) any { return "***" }),
		"User.nick":   Allow(),
	}
	observe := func(opts ...ExecutorOption) map[string]bool {
		got := map[string]bool{}
		opts = append(opts, WithFieldInterceptor(FieldInterceptorFunc(
			func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
				if fc.Field.Name == "me" {
					for f := range fc.Selection().Fields() {
						got[f.Name] = f.Withheld(ctx)
					}
				}
				return next(ctx)
			})))
		run(t, NewExecutor(s, opts...), `{ me { name orders { id } salary email nick } }`, "")
		return got
	}

	got := observe(WithAuthorizer(AuthorizerFunc(func(_ context.Context, shape *AuthShape, d *Decision) error {
		for i, site := range shape.Sites() {
			if o, ok := outcomes[site.Coord]; ok {
				if err := d.Set(i, o); err != nil {
					return err
				}
			}
		}
		return nil
	})))
	want := map[string]bool{"name": false, "orders": true, "salary": true, "email": false, "nick": false}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("Withheld(%s) = %v, want %v", name, got[name], w)
		}
	}

	// No Authorizer: nothing is withheld.
	for name, w := range observe() {
		if w {
			t.Errorf("without an Authorizer, Withheld(%s) = true", name)
		}
	}
}

// The Decision rides on the operation's own context node at no cost; a
// context without that node for this operation -- one an operation
// interceptor rebuilt -- still carries it, on a node of its own.
func TestWithDecisionFallsBackToAContextValue(t *testing.T) {
	d := &Decision{}
	oc := &OperationContext{}

	ctx := withOperation(context.Background(), oc)
	if got := withDecision(ctx, oc, d); got != ctx {
		t.Error("the operation's own node should carry the Decision without a new context")
	}
	if got, _ := ctx.Value(decisionCtxKey{}).(*Decision); got != d {
		t.Error("Decision not visible through the operation's node")
	}

	other := withOperation(context.Background(), &OperationContext{})
	got, _ := withDecision(other, oc, d).Value(decisionCtxKey{}).(*Decision)
	if got != d {
		t.Error("a node of another operation must not take the Decision, and the fallback must carry it")
	}
	if v, _ := other.Value(decisionCtxKey{}).(*Decision); v != nil {
		t.Error("another operation's node was given this operation's Decision")
	}
}
