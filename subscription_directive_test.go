package graphql

import (
	"context"
	"strings"
	"testing"
)

const subDirectiveSDL = `
directive @auth(scope: String!) on FIELD_DEFINITION | OBJECT
type Query { ok: String @auth(scope: "a") }
type Subscription { ticks: String @auth(scope: "a") }
`

type subAuthArgs struct {
	Scope string `graphql:"scope"`
}

func subDirectiveOptions(extra ...SchemaOption) []SchemaOption {
	return append([]SchemaOption{
		Args[subAuthArgs](),
		DirectiveArgs("auth", func(next FieldFunc, _ subAuthArgs) FieldFunc { return next }),
		Query(Resolve("ok", func(context.Context, Root) (*string, error) { s := "x"; return &s, nil })),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan *string, error) { return nil, nil })),
	}, extra...)
}

// A directive bound for ordinary fields still cannot wrap a subscription root field, so a
// schema that puts it there is refused unless the application says it enforces the directive
// itself when a subscription opens.
func TestBoundDirectiveOnSubscriptionRootIsRefusedByDefault(t *testing.T) {
	_, err := NewSchema(SDL(subDirectiveSDL), subDirectiveOptions()...)
	if err == nil || !strings.Contains(err.Error(), "never runs") {
		t.Fatalf("want the subscription-root refusal, got %v", err)
	}
}

func TestSubscriptionRootDirectiveLetsTheApplicationOwnTheCheck(t *testing.T) {
	if _, err := NewSchema(SDL(subDirectiveSDL), subDirectiveOptions(SubscriptionRootDirective("auth"))...); err != nil {
		t.Fatalf("a declared subscription-root directive must build: %v", err)
	}
}

// Declaring a directive that is not bound would switch the refusal off for nothing: a typo
// must not leave a subscription unguarded.
func TestSubscriptionRootDirectiveMustNameABoundDirective(t *testing.T) {
	_, err := NewSchema(SDL(subDirectiveSDL), subDirectiveOptions(SubscriptionRootDirective("auht"))...)
	if err == nil || !strings.Contains(err.Error(), "auht") {
		t.Fatalf("want an error naming the unbound directive, got %v", err)
	}
}
