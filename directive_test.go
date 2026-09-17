package graphql

import (
	"context"
	"strings"
	"testing"
)

// A directive wraps fd.anyResolve, and a subscription root field is served
// by fd.subscribe with the per-event writer substituting its executor
// entirely. A bound directive there therefore never runs. It must be a
// build error rather than a silently absent check.
func TestBoundDirectiveOnSubscriptionRootIsRejected(t *testing.T) {
	const sdl = `
directive @guard on FIELD_DEFINITION
type Query { ping: String! }
type Subscription { ticks: Int! @guard }
`
	ch := make(chan int)
	_, err := NewSchema(SDL(sdl),
		Query(Field("ping", func(Root) string { return "pong" })),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan int, error) { return ch, nil })),
		Directive("guard", func(next FieldFunc) FieldFunc { return next }),
	)
	if err == nil {
		t.Fatal("NewSchema accepted a bound directive on a subscription root field")
	}
	for _, want := range []string{"Subscription.ticks", "@guard", "SubscriptionInterceptor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q\n got: %v", want, err)
		}
	}
}

// A bound directive on the Subscription type wraps every one of its fields
// the same way a field-level one does, so it never runs either and must be
// rejected by the same check, naming the field it would have wrapped.
func TestBoundDirectiveOnSubscriptionTypeIsRejected(t *testing.T) {
	const sdl = `
directive @guard on FIELD_DEFINITION | OBJECT
type Query { ping: String! }
type Subscription @guard { ticks: Int! }
`
	ch := make(chan int)
	_, err := NewSchema(SDL(sdl),
		Query(Field("ping", func(Root) string { return "pong" })),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan int, error) { return ch, nil })),
		Directive("guard", func(next FieldFunc) FieldFunc { return next }),
	)
	if err == nil {
		t.Fatal("NewSchema accepted a bound directive on the Subscription type")
	}
	for _, want := range []string{"Subscription.ticks", "@guard", "SubscriptionInterceptor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q\n got: %v", want, err)
		}
	}
}

// The rejection must be about bound directives, not about directives. A
// subscription root carrying only unbound or built-in directives still builds.
func TestUnboundDirectiveOnSubscriptionRootIsAllowed(t *testing.T) {
	const sdl = `
directive @note on FIELD_DEFINITION
type Query { ping: String! }
type Subscription { ticks: Int! @note @deprecated(reason: "x") }
`
	ch := make(chan int)
	if _, err := NewSchema(SDL(sdl),
		Query(Field("ping", func(Root) string { return "pong" })),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan int, error) { return ch, nil })),
	); err != nil {
		t.Fatalf("NewSchema rejected an unbound directive: %v", err)
	}
}
