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

// Error() is what every %v, every log line and every caller that does not
// unwrap will see, and it is the only part of this type nothing exercised.
// Its two branches disagree about what it means to have no errors, and the
// zero-Response one is reachable: a transport constructing a SubscribeError
// to report its own refusal has no Response to put in it.
func TestSubscribeErrorStringifies(t *testing.T) {
	_, e := newFixtureExecutor(t)
	_, err := e.Subscribe(context.Background(), &Request{Query: `subscription { nope }`})
	var se *SubscribeError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *SubscribeError", err)
	}
	if got := se.Error(); got == "" || got == "graphql: " {
		t.Errorf("Error() = %q, which says nothing about the refusal", got)
	}
	if got, want := se.Error(), "graphql: "+se.Response.Errors[0].Message; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	for _, empty := range []*SubscribeError{{}, {Response: &Response{}}} {
		if got := empty.Error(); got == "" {
			t.Errorf("%#v stringifies to the empty string", empty)
		}
	}
}
