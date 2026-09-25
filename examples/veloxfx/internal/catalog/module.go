// Package catalog implements the category and product groups.
//
// A domain package implements its groups the way a gRPC server package
// implements its services: one Resolver type per group, each in
// <group>.resolvers.go, which gqlc scaffolds, and one Module registering them.
package catalog

import (
	"go.uber.org/fx"

	categorygql "github.com/syssam/graphql-go/examples/veloxfx/graph/category"
	productgql "github.com/syssam/graphql-go/examples/veloxfx/graph/product"
)

// Module provides each group's Resolver and contributes its bindings to the
// schema's value group, as RegisterXServer contributes a service to a gRPC
// server. Nothing outside the domain lists its groups.
var Module = fx.Module("catalog",
	fx.Provide(
		fx.Annotate(NewCategoryResolver, fx.As(new(categorygql.Resolver))),
		fx.Annotate(NewProductResolver, fx.As(new(productgql.Resolver))),
		fx.Annotate(categorygql.Bindings, fx.ResultTags(`group:"graphql"`)),
		fx.Annotate(productgql.Bindings, fx.ResultTags(`group:"graphql"`)),
	),
)
