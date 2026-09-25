// Package inventory implements the warehouse and stock groups, and owns the
// one rule about stock every other domain relies on: a count never goes
// below zero. Take and Return (stock.go) are how sales moves it.
package inventory

import (
	"go.uber.org/fx"

	stockgql "github.com/syssam/graphql-go/examples/veloxfx/graph/stock"
	warehousegql "github.com/syssam/graphql-go/examples/veloxfx/graph/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/gqlfx"
)

// Module registers each group; see catalog.Module.
var Module = fx.Module("inventory",
	gqlfx.Register(NewWarehouseResolver, warehousegql.Bindings),
	gqlfx.Register(NewStockResolver, stockgql.Bindings),
)
