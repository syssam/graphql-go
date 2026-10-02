package veloxfx

import (
	"maps"
	"slices"

	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/veloxfx/graph/category"
	"github.com/syssam/graphql-go/examples/veloxfx/graph/customer"
	"github.com/syssam/graphql-go/examples/veloxfx/graph/order"
	"github.com/syssam/graphql-go/examples/veloxfx/graph/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/graph/product"
	"github.com/syssam/graphql-go/examples/veloxfx/graph/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/graph/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/gqlfx"
	customersvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/customer"
	ordersvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/order"
	productsvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/product"
)

// groups registers each entity's Handler over its generated bindings, as a
// gRPC server registers every service, keyed by group so the one list is
// also what a test leaves an entity out of. Each Handler lives in its
// entity's package beside the code gqlc generated for it, graph/<entity>.
var groups = map[string]fx.Option{
	"category":  gqlfx.Register(category.NewHandler, category.Bindings),
	"product":   gqlfx.Register(product.NewHandler, product.Bindings),
	"customer":  gqlfx.Register(customer.NewHandler, customer.Bindings),
	"order":     gqlfx.Register(order.NewHandler, order.Bindings),
	"orderitem": gqlfx.Register(orderitem.NewHandler, orderitem.Bindings),
	"warehouse": gqlfx.Register(warehouse.NewHandler, warehouse.Bindings),
	"stock":     gqlfx.Register(stock.NewHandler, stock.Bindings),
}

// entities are the federation entities, which the services answer.
var entities = fx.Options(
	gqlfx.Entity((*productsvc.Service).Entity),
	gqlfx.Entity((*customersvc.Service).Entity),
	gqlfx.Entity((*ordersvc.Service).Entity),
)

// Resolvers is every group and every federation entity.
var Resolvers = fx.Module("resolver", resolverOptions()...)

// resolverOptions is the groups, in name order, but those named in without,
// and the entities.
func resolverOptions(without ...string) []fx.Option {
	var opts []fx.Option
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		if !slices.Contains(without, g) {
			opts = append(opts, groups[g])
		}
	}
	return append(opts, entities)
}
