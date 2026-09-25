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
		edge.To("orders", Order.Type),
	}
}

func (Customer) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate()),
	}
}

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
		edge.To("items", OrderItem.Type),
	}
}

func (Order) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}

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
		graphql.Mutations(graphql.MutationCreate()),
	}
}
