// Package inventory implements the warehouse and stock groups, and owns the
// one rule about stock every other domain relies on: a count never goes
// below zero. Take and Return (stock.go) are how sales moves it.
package inventory

import (
	"go.uber.org/fx"

	stockgql "github.com/syssam/graphql-go/examples/veloxfx/graph/stock"
	warehousegql "github.com/syssam/graphql-go/examples/veloxfx/graph/warehouse"
)

// Module provides each group's Resolver and contributes its bindings to the
// schema; see catalog.Module.
var Module = fx.Module("inventory",
	fx.Provide(
		fx.Annotate(NewWarehouseResolver, fx.As(new(warehousegql.Resolver))),
		fx.Annotate(NewStockResolver, fx.As(new(stockgql.Resolver))),
		fx.Annotate(warehousegql.Bindings, fx.ResultTags(`group:"graphql"`)),
		fx.Annotate(stockgql.Bindings, fx.ResultTags(`group:"graphql"`)),
	),
)
