package veloxfx

import (
	"context"
	"errors"
	"log/slog"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/apperr"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/velox/dialect/sql/sqlgraph"
)

// presentError is what a client sees of an error, and the one place that
// decides it, as a gRPC server maps errors to status codes. Every error
// carries extensions.code, one of internal/apperr's, so a client branches on
// the code and never on message text.
//
// A service raises the codes that are its decision with apperr.New; this maps
// what velox raises onto the same codes, and masks the rest so a driver's
// message never reaches a client.
func presentError(ctx context.Context, err error) *graphql.Error {
	var gerr *graphql.Error
	switch {
	case errors.As(err, &gerr):
		return graphql.DefaultErrorPresenter(ctx, err)
	case velox.IsNotFound(err):
		return apperr.Wrap(err, apperr.NotFound, "not found")
	case velox.IsValidationError(err):
		return apperr.Wrap(err, apperr.BadUserInput, "%s", err.Error())
	// By SQLSTATE or driver code, not message text: SQLite says "FOREIGN
	// KEY constraint failed" and PostgreSQL "violates foreign key
	// constraint", and matching the first classified every PostgreSQL
	// delete of a referenced row as a duplicate.
	case sqlgraph.IsForeignKeyConstraintError(err):
		return apperr.Wrap(err, apperr.FailedPrecondition, "still referenced; delete what refers to it first")
	// A validator's bound, enforced by the database (FeatureCheckBounds):
	// what an AddX past it reaches, since no validator sees an addition.
	case sqlgraph.IsCheckConstraintError(err):
		return apperr.Wrap(err, apperr.FailedPrecondition, "the change would take a value past its limit")
	case velox.IsConstraintError(err):
		return apperr.Wrap(err, apperr.Conflict, "already exists")
	}
	slog.ErrorContext(ctx, "graphql: internal error", "error", err)
	return apperr.Wrap(err, apperr.Internal, "internal error")
}
