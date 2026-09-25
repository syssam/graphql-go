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
	"github.com/syssam/graphql-go/examples/veloxfx/internal/gqlfx"
)

// Module registers each group, as a gRPC server registers each service.
// Nothing outside the domain lists its groups.
var Module = fx.Module("catalog",
	gqlfx.Register(NewCategoryResolver, categorygql.Bindings),
	gqlfx.Register(NewProductResolver, productgql.Bindings),
)
