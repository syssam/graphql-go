// Package apperr is every error code a client of this service can branch on,
// and the one way to raise one -- what codes and status are to a gRPC server.
// A client branches on extensions.code, never on message text, so a code is a
// contract: add one here, never spell one inline.
package apperr

import (
	"errors"
	"fmt"
	"maps"

	graphql "github.com/syssam/graphql-go"
)

// The codes. Those the engine also raises are its own constants, so the two
// can never drift apart.
const (
	// NotFound: the id named no row, or none the viewer may see.
	NotFound = "NOT_FOUND"
	// BadUserInput: the input broke a rule -- a field rule velox enforces, or
	// an operation's own check.
	BadUserInput = graphql.CodeBadUserInput
	// Conflict: a unique column already holds the value.
	Conflict = "CONFLICT"
	// FailedPrecondition: the state forbids it -- a row still referenced, an
	// order in the wrong status, not enough stock.
	FailedPrecondition = "FAILED_PRECONDITION"
	// Unauthenticated: nobody is signed in, and this needs someone.
	Unauthenticated = "UNAUTHENTICATED"
	// Forbidden: the viewer is signed in and may not do this.
	Forbidden = graphql.CodeForbidden
	// Internal: anything else; its message is fixed and the cause is logged.
	Internal = graphql.CodeInternal
)

// New is an error a client sees as its message, with extensions.code set.
func New(code, format string, args ...any) *graphql.Error {
	return graphql.Errorf(format, args...).WithCode(code)
}

// Wrap is New with a cause, kept for logging and errors.Is and never sent.
func Wrap(err error, code, format string, args ...any) *graphql.Error {
	e := New(code, format, args...)
	e.Err = err
	return e
}

// Prefixed puts where in front of a coded error's message and keeps its code
// and cause, so "not enough in stock" can say which line of an order it was.
// Any other error is returned as it is, for the presenter to classify.
//
// The result is an error of its own: its extensions are a copy, since the
// coded error may be a package-level one every request returns, and its
// cause is err whole, so anything err carried around the coded error -- a
// failed rollback joined beside it -- is still found by errors.Is.
func Prefixed(where string, err error) error {
	var gerr *graphql.Error
	if !errors.As(err, &gerr) {
		return err
	}
	out := *gerr
	out.Message = fmt.Sprintf("%s: %s", where, gerr.Message)
	out.Extensions = maps.Clone(gerr.Extensions)
	out.Err = err
	return &out
}
