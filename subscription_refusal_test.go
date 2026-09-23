package graphql

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Subscribe refuses a bad request through five branches that nothing drove:
// operation selection, malformed variables JSON, variable coercion, plan
// errors, and the argument decode. Each returns a *SubscribeError wrapping a
// whole *Response, and that shape is load-bearing for the streaming
// transports: gqlws turns Response.Errors into a protocol-level `error`
// message, so an empty or wrongly-coded Response there is what a client sees
// instead of a reason.
//
// The equivalent Execute paths are all covered. These are the same failures
// asked of a different entry point, and that is exactly where two
// implementations of one rule drift.
func TestSubscribeRefusesABadRequest(t *testing.T) {
	_, e := newSubExecutor(t)
	for _, c := range []struct {
		name, query, vars, wantCode, wantMsg string
	}{
		{
			"not a subscription operation",
			`query { ping }`, "",
			CodeOperationResolution, "requires a subscription operation",
		},
		{
			"unknown operation name",
			`subscription A { counter }`, "",
			"", "",
		},
		{
			"malformed variables JSON",
			`subscription($n: Int!){ countdown(from: $n) }`, `{"n":`,
			CodeBadUserInput, "",
		},
		{
			"variable of the wrong type",
			`subscription($n: Int!){ countdown(from: $n) }`, `{"n":"no"}`,
			CodeBadUserInput, "Int",
		},
		{
			"required variable missing",
			`subscription($n: Int!){ countdown(from: $n) }`, `{}`,
			CodeBadUserInput, "was not provided",
		},
		{
			"field that does not exist",
			`subscription { nope }`, "",
			"", "nope",
		},
		{
			"validation failure inside the selection",
			`subscription { messages { nope } }`, "",
			"", "nope",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &Request{Query: c.query}
			if c.vars != "" {
				req.Variables = []byte(c.vars)
			}
			if c.name == "unknown operation name" {
				req.OperationName = "B"
			}
			ch, err := e.Subscribe(context.Background(), req)
			if err == nil {
				t.Fatalf("the request was accepted; ch = %v", ch)
			}
			if ch != nil {
				t.Errorf("a refused subscription still returned a channel")
			}

			var se *SubscribeError
			if !errors.As(err, &se) {
				t.Fatalf("error is %T, want a *SubscribeError -- the transports read "+
					"Response off it to build their protocol error", err)
			}
			if se.Response == nil || len(se.Response.Errors) == 0 {
				t.Fatalf("SubscribeError carries no errors, so a client is told a "+
					"subscription failed and not why: %#v", se.Response)
			}
			// errors.As must reach the *Error the ordinary way, which is what
			// SubscribeError.Unwrap exists for.
			var ge *Error
			if !errors.As(err, &ge) {
				t.Fatalf("errors.As could not reach the *Error through %T", err)
			}
			if c.wantCode != "" {
				if got := se.Response.Errors[0].Extensions["code"]; got != c.wantCode {
					t.Errorf("code = %v, want %s (message: %s)",
						got, c.wantCode, se.Response.Errors[0].Message)
				}
			}
			if c.wantMsg != "" && !strings.Contains(se.Response.Errors[0].Message, c.wantMsg) {
				t.Errorf("message %q does not contain %q", se.Response.Errors[0].Message, c.wantMsg)
			}
			// And Error() says something, since a caller that does not unwrap
			// sees only this.
			if s := err.Error(); s == "" || s == "graphql: " {
				t.Errorf("Error() = %q", s)
			}
		})
	}
}

// Subscribe and Execute are two entry points onto one set of rules. A
// document that Execute rejects for a reason unrelated to the operation kind
// must be rejected by Subscribe with the same code, or a client gets a
// different answer depending on which verb it used.
func TestSubscribeAndExecuteAgreeOnABadVariable(t *testing.T) {
	_, e := newSubExecutor(t)
	const vars = `{"n":"no"}`

	_, serr := e.Subscribe(context.Background(), &Request{
		Query:     `subscription($n: Int!){ countdown(from: $n) }`,
		Variables: []byte(vars),
	})
	if serr == nil {
		t.Fatal("Subscribe accepted a string for Int!")
	}
	var se *SubscribeError
	if !errors.As(serr, &se) {
		t.Fatalf("error is %T", serr)
	}

	// The variable has to be *used*, and at a comparable position: a declared
	// but unused variable is a validation failure that fires first, which is
	// correct and is not the rule being compared. subSDL's Query has no field
	// taking an Int, so the query side runs on the root fixture -- different
	// schema, same rule.
	_, qe := newFixtureExecutor(t)
	exec := qe.Execute(context.Background(), &Request{
		Query:     `query($n: Int!){ echo(v: $n) }`,
		Variables: []byte(vars),
	})
	if len(exec.Errors) == 0 {
		t.Fatal("Execute accepted a string for Int!")
	}

	sCode := se.Response.Errors[0].Extensions["code"]
	eCode := exec.Errors[0].Extensions["code"]
	if sCode != eCode {
		t.Errorf("Subscribe reports %v and Execute %v for the same bad variable", sCode, eCode)
	}
}
