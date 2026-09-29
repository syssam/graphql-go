package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	graphql "github.com/syssam/graphql-go"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/apperr"
)

var errCause = errors.New("driver said something private")

func code(e *graphql.Error) any { return e.Extensions["code"] }

// Wrap keeps the cause for errors.Is and the log, and sends only its own
// message: a driver's text must not reach a client through it.
func TestWrapKeepsTheCauseOutOfTheMessage(t *testing.T) {
	e := apperr.Wrap(errCause, apperr.Conflict, "already exists")
	if e.Message != "already exists" || code(e) != "CONFLICT" {
		t.Errorf("Wrap = %q %v", e.Message, code(e))
	}
	if !errors.Is(e, errCause) {
		t.Error("Wrap lost its cause")
	}
}

// Prefixed says where, and keeps everything else: the code a client branches
// on, the cause errors.Is finds, and the original error untouched.
func TestPrefixedKeepsCodeAndCause(t *testing.T) {
	orig := apperr.Wrap(errCause, apperr.FailedPrecondition, "not enough in stock")
	got := apperr.Prefixed("items[1]", fmt.Errorf("take: %w", orig))
	var gerr *graphql.Error
	if !errors.As(got, &gerr) {
		t.Fatalf("Prefixed = %T %v", got, got)
	}
	if gerr.Message != "items[1]: not enough in stock" || code(gerr) != "FAILED_PRECONDITION" || !errors.Is(got, errCause) {
		t.Errorf("Prefixed = %q %v, cause kept %v", gerr.Message, code(gerr), errors.Is(got, errCause))
	}
	if orig.Message != "not enough in stock" {
		t.Errorf("Prefixed changed the error it was given: %q", orig.Message)
	}
	// The copy is its own error: a shared original -- a package-level error
	// many requests return -- must not see extensions set on it.
	gerr.WithExtension("where", "items[1]")
	if _, leaked := orig.Extensions["where"]; leaked {
		t.Error("an extension set on the prefixed error reached the original")
	}
	// And nothing around the coded error is lost: a sentinel joined beside it
	// by a transaction's rollback is still found.
	errRollback := errors.New("rollback failed")
	if joined := apperr.Prefixed("x", errors.Join(orig, errRollback)); !errors.Is(joined, errRollback) {
		t.Error("Prefixed dropped the error chain around the coded error")
	}
	// An error with no code is left for the presenter to classify.
	if plain := apperr.Prefixed("items[1]", errCause); plain != errCause { //nolint:errorlint // identity is the claim
		t.Errorf("Prefixed wrapped an uncoded error: %v", plain)
	}
}
