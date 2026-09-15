package benchmarks

import (
	"context"
	"encoding/json"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/executor"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/benchmarks/graph"
	"github.com/syssam/graphql-go/benchmarks/internal/data"
)

type gqlgenRunner struct {
	ex *executor.Executor
}

func newGQLGen(users []*data.User) *gqlgenRunner {
	es := graph.NewExecutableSchema(graph.Config{Resolvers: &graph.Resolver{Data: users}})
	ex := executor.New(es)
	ex.SetQueryCache(lru.New[*ast.QueryDocument](1024))
	return &gqlgenRunner{ex: ex}
}

func (r *gqlgenRunner) run(ctx context.Context, query string) *graphql.Response {
	ctx = graphql.StartOperationTrace(ctx)
	opCtx, errs := r.ex.CreateOperationContext(ctx, &graphql.RawParams{Query: query})
	if len(errs) > 0 {
		panic(errs)
	}
	h, ctx := r.ex.DispatchOperation(ctx, opCtx)
	resp := h(ctx)
	if resp == nil {
		panic("nil gqlgen response")
	}
	return resp
}

func (r *gqlgenRunner) execute(ctx context.Context, query string) []byte {
	resp := r.run(ctx, query)
	if len(resp.Errors) > 0 {
		panic(resp.Errors)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	return b
}
