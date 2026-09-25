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
)

// Module provides each group's Resolver and contributes its bindings to the
// schema; see catalog.Module.
var Module = fx.Module("sales",
	fx.Provide(
		fx.Annotate(NewCustomerResolver, fx.As(new(customergql.Resolver))),
		fx.Annotate(NewOrderResolver, fx.As(new(ordergql.Resolver))),
		fx.Annotate(NewOrderItemResolver, fx.As(new(orderitemgql.Resolver))),
		fx.Annotate(customergql.Bindings, fx.ResultTags(`group:"graphql"`)),
		fx.Annotate(ordergql.Bindings, fx.ResultTags(`group:"graphql"`)),
		fx.Annotate(orderitemgql.Bindings, fx.ResultTags(`group:"graphql"`)),
	),
)
