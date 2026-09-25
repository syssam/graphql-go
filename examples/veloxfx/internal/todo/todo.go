// Package todo implements the todo group's Resolver: the Todo type's
// resolver fields and the root fields returning a Todo.
package todo

import (
	"context"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	gen "github.com/syssam/graphql-go/examples/veloxfx/graph/todo"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Module provides the resolver and registers its bindings with the schema.
var Module = fx.Module("todo",
	fx.Provide(New),
	resolve.Bindings(func(r *Resolver) graphql.SchemaOption { return gen.Bindings(r) }),
)

type Resolver struct {
	client *velox.Client
}

var _ gen.Resolver = (*Resolver)(nil)

func New(client *velox.Client) *Resolver { return &Resolver{client: client} }

// Todos eager-loads the owner, because velox's Todo.Owner otherwise queries
// once per row.
func (r *Resolver) Todos(ctx context.Context) ([]*entity.Todo, error) {
	return resolve.List(r.client.Todo.Query().WithOwner().All(ctx))
}

// CreateTodo reads the row back rather than returning what Save returned.
// velox's create marks the owner edge loaded with a stub holding only the id
// (&entity.User{ID: ownerID}), so Todo.Owner would answer `owner { name }`
// with an empty name instead of querying for it.
func (r *Resolver) CreateTodo(ctx context.Context, args gen.CreateTodoArgs) (*entity.Todo, error) {
	t, err := r.client.Todo.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.Todo.Get(ctx, t.ID)
}

func (r *Resolver) UpdateTodo(ctx context.Context, args gen.UpdateTodoArgs) (*entity.Todo, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Todo.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

func (r *Resolver) TodoID(_ context.Context, t *entity.Todo) (graphql.ID, error) {
	return resolve.ID(t.ID), nil
}
