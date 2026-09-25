// Package sales implements the customer, order and orderitem groups.
//
// An order changes only through the operations in sdl/order.graphql, and
// each one is a conditional update on the state it moves from: two requests
// racing to ship and cancel the same order cannot both win, because the
// second one's WHERE status = ... matches no row.
package sales

import (
	"go.uber.org/fx"

	customergql "github.com/syssam/graphql-go/examples/veloxfx/graph/customer"
	ordergql "github.com/syssam/graphql-go/examples/veloxfx/graph/order"
	orderitemgql "github.com/syssam/graphql-go/examples/veloxfx/graph/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/gqlfx"
)

// Module registers each group; see catalog.Module.
var Module = fx.Module("sales",
	gqlfx.Register(NewCustomerResolver, customergql.Bindings),
	gqlfx.Register(NewOrderResolver, ordergql.Bindings),
	gqlfx.Register(NewOrderItemResolver, orderitemgql.Bindings),
)
