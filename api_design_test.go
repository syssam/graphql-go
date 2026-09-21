package graphql

import (
	"context"
	"errors"
	"testing"
)

// A subscription that is refused before its stream opens returns a
// *SubscribeError carrying a whole Response, because the refusal can name
// several errors. A caller should still be able to reach them the ordinary
// way: errors.As for a *Error, errors.Is for a sentinel. Without Unwrap the
// chain stops at SubscribeError and every caller has to know to reach through
// .Response.Errors by hand.
func TestSubscribeErrorUnwrapsItsErrors(t *testing.T) {
	_, e := newFixtureExecutor(t)
	_, err := e.Subscribe(context.Background(), &Request{Query: `subscription { nope }`})
	if err == nil {
		t.Fatal("want an error for an invalid subscription")
	}
	var se *SubscribeError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *SubscribeError", err)
	}
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("errors.As could not reach the GraphQL error through %T", err)
	}
	if ge.Message == "" {
		t.Fatal("unwrapped error carries no message")
	}
	if len(se.Response.Errors) > 0 && ge != se.Response.Errors[0] {
		t.Fatal("errors.As did not return the response's first error")
	}
}

// FieldOpt is deprecated but must stay an alias rather than become a defined
// type: existing code passes a FieldOpt where a FieldSchedule is wanted and
// vice versa, and only an alias keeps both directions compiling.
func TestFieldOptRemainsAnAliasOfFieldSchedule(t *testing.T) {
	// The explicit types are the assertion: a defined type would reject one
	// of these assignments. Inference would make the test prove nothing.
	var sched FieldSchedule = Inline() //nolint:staticcheck // QF1011: the explicit type is the point
	var opt FieldOpt = sched           //nolint:staticcheck // QF1011: the explicit type is the point
	sched = opt
	// Assignable in both directions with no conversion, which a defined type
	// would refuse.
	if sched == nil {
		t.Fatal("Inline() returned nil")
	}
	// And the constructors accept either spelling.
	_ = Field("x", func(Root) string { return "" }, opt)
	_ = Field("y", func(Root) string { return "" }, sched)
}
