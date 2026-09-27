package veloxfx

import (
	"database/sql"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	gqlotel "github.com/syssam/graphql-go/ext/otel"
)

// Tracing is on when the process provides a TracerProvider:
//
//	fx.Supply(fx.Annotate(tp, fx.As(new(trace.TracerProvider))))
//
// Every operation is then a span, every resolver a span beneath it, and
// every SQL statement a span beneath the resolver that ran it -- which is
// what says which field made a page slow. Pure fields get none: they read
// what a resolver already loaded, and a span per column of every row would
// bury the ones that matter.
type Tracing struct {
	fx.In
	TracerProvider trace.TracerProvider `optional:"true"`
}

// openSQL opens the database, through otelsql when tracing is on: velox runs
// every statement through database/sql, so instrumenting there sees them
// all, eager loads included, under the context of the resolver that asked.
func openSQL(driver, dsn string, tr Tracing) (*sql.DB, error) {
	if tr.TracerProvider == nil {
		return sql.Open(driver, dsn)
	}
	return otelsql.Open(driver, dsn, otelsql.WithTracerProvider(tr.TracerProvider))
}

// tracingOptions go first, so the operation span covers the authorizer and
// every other interceptor too.
func tracingOptions(tr Tracing) []graphql.ExecutorOption {
	if tr.TracerProvider == nil {
		return nil
	}
	return gqlotel.New(gqlotel.WithTracerProvider(tr.TracerProvider), gqlotel.WithResolverSpans(true))
}
