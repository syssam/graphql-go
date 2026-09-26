package schema

import (
	"github.com/syssam/velox"
	"github.com/syssam/velox/contrib/graphql"
	"github.com/syssam/velox/schema"
	"github.com/syssam/velox/schema/edge"
	"github.com/syssam/velox/schema/field"
	"github.com/syssam/velox/schema/mixin"
)

type Customer struct{ velox.Schema }

func (Customer) Fields() []velox.Field {
	return []velox.Field{
		field.String("name").NotEmpty(),
		field.String("email").NotEmpty().Unique(),
	}
}

func (Customer) Edges() []velox.Edge {
	return []velox.Edge{
		edge.To("orders", Order.Type).Annotations(graphql.Skip(graphql.SkipInputs)),
	}
}

func (Customer) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}

// Order has no velox mutations. It is created by placeOrder and moves
// through its states by payOrder, shipOrder and cancelOrder (sdl/order.graphql),
// each of which checks the state it moves from.
type Order struct{ velox.Schema }

func (Order) Mixin() []velox.Mixin {
	return []velox.Mixin{mixin.Time{}}
}

func (Order) Fields() []velox.Field {
	return []velox.Field{
		field.Enum("status").Values("PENDING", "PAID", "SHIPPED", "CANCELLED").Default("PENDING"),
	}
}

func (Order) Edges() []velox.Edge {
	return []velox.Edge{
		edge.From("customer", Customer.Type).Ref("orders").Unique().Required(),
		// The warehouse the items were taken from, which is where a
		// cancellation puts them back.
		edge.From("warehouse", Warehouse.Type).Ref("orders").Unique().Required(),
		edge.To("items", OrderItem.Type),
	}
}

func (Order) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.RelayConnection(),
		graphql.WhereInputFields("status"),
		graphql.WhereInputEdges("customer"),
		graphql.QueryField(),
		// totalCents is computed, not stored: its resolver sums every item.
		// Loads is what makes velox load the items whole whenever it is
		// selected, whatever the client selected beneath items.
		graphql.Resolvers(
			graphql.Map("totalCents", "Int!").Loads("items").
				WithComment("Sum of quantity times unit price over the items."),
		),
	}
}

// OrderItem has no mutations: a line is written by placeOrder, at the price
// the product had then, and never edited.
type OrderItem struct{ velox.Schema }

func (OrderItem) Fields() []velox.Field {
	return []velox.Field{
		field.Int("quantity").Positive(),
		field.Int("unit_price_cents").NonNegative(),
	}
}

func (OrderItem) Edges() []velox.Edge {
	return []velox.Edge{
		edge.From("order", Order.Type).Ref("items").Unique().Required(),
		edge.From("product", Product.Type).Ref("order_items").Unique().Required(),
	}
}

func (OrderItem) Annotations() []schema.Annotation {
	return []schema.Annotation{
		// An order's items are a plain list, not a connection; see Stock.
		graphql.QueryField(),
	}
}
