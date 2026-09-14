// Package compare runs graphql-go and gqlgen over one shared schema, one
// shared set of Go structs and one shared dataset, so a difference in the
// numbers is a difference between the engines.
//
// The generated halves are written by `go run gen.go` and are not committed;
// gqlgen emits several hundred thousand lines at a realistic entity count.
package compare

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	gqlgengraphql "github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/executor"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	theirgen "github.com/syssam/graphql-go/compare/gqlgen/gen"
	"github.com/syssam/graphql-go/compare/gqlgen/gen/exec"
	ourgen "github.com/syssam/graphql-go/compare/graphqlgo/gen"
)

// NewGraphQLGo builds the graphql-go executor over the generated bindings.
func NewGraphQLGo() (*graphql.Executor, error) {
	s, err := ourgen.NewSchema(ourgen.All(),
		graphql.Scalar("Time",
			func(w *graphql.Writer, t time.Time) error {
				w.String(t.Format(time.RFC3339Nano))
				return nil
			},
			func(raw any) (time.Time, error) {
				s, ok := raw.(string)
				if !ok {
					return time.Time{}, fmt.Errorf("Time must be a string")
				}
				return time.Parse(time.RFC3339Nano, s)
			},
		),
	)
	if err != nil {
		return nil, err
	}
	return graphql.NewExecutor(s), nil
}

// GraphQLGoJSON executes a query and returns the response body.
func GraphQLGoJSON(e *graphql.Executor, query string) ([]byte, error) {
	resp := e.Execute(context.Background(), &graphql.Request{Query: query})
	if len(resp.Errors) > 0 {
		b, _ := json.Marshal(resp.Errors)
		return nil, fmt.Errorf("graphql-go: %s", b)
	}
	return resp.Data, nil
}

// NewGqlgen builds the gqlgen executor over the same resolvers, with the same
// document cache size graphql-go uses by default.
func NewGqlgen() *executor.Executor {
	es := exec.NewExecutableSchema(exec.Config{Resolvers: &theirgen.Resolver{}})
	ex := executor.New(es)
	ex.SetQueryCache(lru.New[*ast.QueryDocument](1024))
	// The raw executor leaves introspection off; the handler package normally
	// adds this. graphql-go answers introspection out of the box, so enable it
	// here or the two are not being asked the same question.
	ex.Use(extension.Introspection{})
	return ex
}

// GqlgenJSON executes a query and returns the data half of the response, so
// both engines are compared on the same bytes.
func GqlgenJSON(ex *executor.Executor, query string) ([]byte, error) {
	ctx := gqlgengraphql.StartOperationTrace(context.Background())
	opCtx, errs := ex.CreateOperationContext(ctx, &gqlgengraphql.RawParams{Query: query})
	if len(errs) > 0 {
		return nil, fmt.Errorf("gqlgen: %v", errs)
	}
	h, ctx := ex.DispatchOperation(ctx, opCtx)
	resp := h(ctx)
	if resp == nil {
		return nil, fmt.Errorf("gqlgen: nil response")
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("gqlgen: %v", resp.Errors)
	}
	return resp.Data, nil
}
