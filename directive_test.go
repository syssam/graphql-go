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

// Order is part of what a directive means: @auth @cache must refuse before the
// cache answers. A field's directives wrap in the order written, the first
// outermost, and the object's wrap all of them -- a guard on the type runs
// before anything written on its fields.
func TestDirectivesWrapInWrittenOrderWithTheObjectsOutermost(t *testing.T) {
	var trace []string
	traced := func(name string) SchemaOption {
		return Directive(name, func(next FieldFunc) FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				trace = append(trace, name)
				return next(ctx, parent, args)
			}
		})
	}
	s, err := NewSchema(SDL(`
directive @a on FIELD_DEFINITION | OBJECT
directive @b on FIELD_DEFINITION | OBJECT
directive @c on FIELD_DEFINITION | OBJECT
directive @d on FIELD_DEFINITION | OBJECT
type T @c @d { x: String @a @b }
type Query { t: T }
`),
		traced("a"), traced("b"), traced("c"), traced("d"),
		Object[struct{}]("T", Field("x", func(*struct{}) *string { trace = append(trace, "x"); return nil })),
		Query(Field("t", func(Root) *struct{} { return &struct{}{} })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ t { x } }`, ""), `{"t":{"x":null}}`)
	if got := strings.Join(trace, " "); got != "c d a b x" {
		t.Fatalf("ran in the order %q, want %q", got, "c d a b x")
	}
}

// A directive that withholds a field answers nil, whatever the field's Go
// type is. For a scalar that is null where the schema allows one and the
// usual error where it does not -- not a complaint that nil is no string.
func TestADirectiveMayAnswerALeafFieldWithNil(t *testing.T) {
	s, err := NewSchema(SDL(`
directive @hide on FIELD_DEFINITION
type Box { required: String! @hide }
type Query { ok: String! secret: String @hide box: Box }
`),
		Directive("hide", func(FieldFunc) FieldFunc {
			return func(context.Context, any, any) (any, error) { return nil, nil }
		}),
		Object[struct{}]("Box", Field("required", func(*struct{}) string { return "shown" })),
		Query(
			Field("ok", func(Root) string { return "x" }),
			Field("secret", func(Root) string { return "shown" }),
			Field("box", func(Root) *struct{} { return &struct{}{} }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	expectData(t, run(t, e, `{ ok secret }`, ""), `{"ok":"x","secret":null}`)
	expectError(t, run(t, e, `{ ok box { required } }`, ""), `{"ok":"x","box":null}`, "box.required",
		"Cannot return null for non-nullable field Box.required.")
}
