package graphql

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A schema that is wrong in many places used to print one line per error: a
// real one reached 33 924, which no terminal and no reader gets through. The
// errors are all still there to walk -- errors.Join's Unwrap contract is what
// callers and errors.Is depend on -- but the rendered message stops.
func TestBuildErrorsAreCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("type Query { a: T0 }\n")
	for i := range 60 {
		fmt.Fprintf(&sb, "type T%d { id: ID! }\n", i)
	}
	_, err := NewSchema(SDL(sb.String()))
	if err == nil {
		t.Fatal("a schema with no bindings at all was accepted")
	}
	msg := err.Error()

	// Every error is still reachable, so nothing is lost by not printing it.
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatal("the build error does not unwrap to its parts")
	}
	all := joined.Unwrap()
	if len(all) < 60 {
		t.Fatalf("only %d errors were kept, want at least 60", len(all))
	}

	// The message says how many there are...
	if !strings.Contains(msg, fmt.Sprint(len(all))) {
		t.Errorf("the message does not say how many errors there are:\n%s", msg)
	}
	// ...and does not print them all.
	if lines := strings.Count(msg, "\n"); lines > 30 {
		t.Errorf("the message printed %d lines; it should stop:\n%s", lines, msg)
	}
	if !strings.Contains(msg, "more") {
		t.Errorf("the message does not say that it stopped:\n%s", msg)
	}
}

// A schema with a handful of problems must read exactly as it did: the cap is
// a guard against a wall, not a new format for every error.
func TestFewBuildErrorsAreUnchanged(t *testing.T) {
	_, err := NewSchema(SDL("type Query { a: T }\ntype T { id: ID! }\n"))
	if err == nil {
		t.Fatal("an unbound schema was accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, "more") || strings.Contains(msg, "build errors") {
		t.Errorf("a small error list was reformatted:\n%s", msg)
	}
	if !strings.HasPrefix(msg, "graphql: ") {
		t.Errorf("the first line is no longer the first error:\n%s", msg)
	}
}
