// Package gqlfx registers a domain's GraphQL groups with fx, as
// RegisterXServer registers a service with a gRPC server.
package gqlfx

import (
	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
)

// Register provides a group's Resolver from newResolver and contributes the
// group's bindings over it to the schema. The Resolver interface is taken
// from bindings, so the two cannot name different groups; that newResolver's
// result implements it is checked when the app is built, before it starts.
//
// The Resolver is provided on its own, rather than passed straight to
// bindings, so a test can fx.Decorate or fx.Replace it.
func Register[R any](newResolver any, bindings func(R) graphql.SchemaOption) fx.Option {
	return fx.Provide(
		fx.Annotate(newResolver, fx.As(new(R))),
		fx.Annotate(bindings, fx.ResultTags(`group:"graphql"`)),
	)
}

// Groups is every group's bindings, as the domain modules Register them.
type Groups struct {
	fx.In
	Bindings []graphql.SchemaOption `group:"graphql"`
}
