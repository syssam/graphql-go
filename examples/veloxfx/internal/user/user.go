// Package user implements the user group's Resolver: the User type's
// resolver fields and the root fields returning a User.
package user

import (
	"context"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	gen "github.com/syssam/graphql-go/examples/veloxfx/graph/user"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Module provides the resolver and registers its bindings with the schema.
var Module = fx.Module("user",
	fx.Provide(New),
	resolve.Bindings(func(r *Resolver) graphql.SchemaOption { return gen.Bindings(r) }),
)

type Resolver struct {
	client *velox.Client
}

var _ gen.Resolver = (*Resolver)(nil)

func New(client *velox.Client) *Resolver { return &Resolver{client: client} }

// Users eager-loads todos, because velox's User.Todos otherwise queries once
// per row.
func (r *Resolver) Users(ctx context.Context) ([]*entity.User, error) {
	return resolve.List(r.client.User.Query().WithTodos().All(ctx))
}

func (r *Resolver) CreateUser(ctx context.Context, args gen.CreateUserArgs) (*entity.User, error) {
	return r.client.User.Create().SetInput(args.Input).Save(ctx)
}

func (r *Resolver) UserID(_ context.Context, u *entity.User) (graphql.ID, error) {
	return resolve.ID(u.ID), nil
}
