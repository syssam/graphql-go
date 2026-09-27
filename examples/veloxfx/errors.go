package veloxfx

import (
	"context"
	"errors"
	"log/slog"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/velox/dialect/sql/sqlgraph"
)

// presentError is what a client sees of an error, and the one place that
// decides it, as a gRPC server maps errors to status codes. Every error
// carries extensions.code, so a client branches on the code and never on
// message text:
//
//	NOT_FOUND            the id named no row
//	BAD_USER_INPUT       the input broke a field rule velox enforces
//	CONFLICT             a unique column already holds the value
//	FAILED_PRECONDITION  the state forbids it: a row still referenced, an
//	                     order in the wrong status, not enough stock
//	INTERNAL_SERVER_ERROR anything else, whose text is logged and not sent
//
// Domain code raises the codes that are its decision as *graphql.Error with
// the code already attached; this maps what velox raises, and masks the rest
// so a driver's message never reaches a client.
func presentError(ctx context.Context, err error) *graphql.Error {
	var gerr *graphql.Error
	switch {
	case errors.As(err, &gerr):
		return graphql.DefaultErrorPresenter(ctx, err)
	case velox.IsNotFound(err):
		return coded(err, "NOT_FOUND", "not found")
	case velox.IsValidationError(err):
		return coded(err, "BAD_USER_INPUT", err.Error())
	// By SQLSTATE or driver code, not message text: SQLite says "FOREIGN
	// KEY constraint failed" and PostgreSQL "violates foreign key
	// constraint", and matching the first classified every PostgreSQL
	// delete of a referenced row as a duplicate.
	case sqlgraph.IsForeignKeyConstraintError(err):
		return coded(err, "FAILED_PRECONDITION", "still referenced; delete what refers to it first")
	// A validator's bound, enforced by the database (FeatureCheckBounds):
	// what an AddX past it reaches, since no validator sees an addition.
	case sqlgraph.IsCheckConstraintError(err):
		return coded(err, "FAILED_PRECONDITION", "the change would take a value past its limit")
	case velox.IsConstraintError(err):
		return coded(err, "CONFLICT", "already exists")
	}
	slog.ErrorContext(ctx, "graphql: internal error", "error", err)
	return coded(err, "INTERNAL_SERVER_ERROR", "internal error")
}

func coded(err error, code, message string) *graphql.Error {
	return (&graphql.Error{Message: message, Err: err}).WithExtension("code", code)
}
