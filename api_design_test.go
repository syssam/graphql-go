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
