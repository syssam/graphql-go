package schema

import (
	"github.com/syssam/velox"
	"github.com/syssam/velox/contrib/graphql"
	"github.com/syssam/velox/schema"
	"github.com/syssam/velox/schema/edge"
	"github.com/syssam/velox/schema/field"
	"github.com/syssam/velox/schema/index"
)

type Warehouse struct{ velox.Schema }

func (Warehouse) Fields() []velox.Field {
	return []velox.Field{
		field.String("name").NotEmpty().Unique(),
	}
}

func (Warehouse) Edges() []velox.Edge {
	return []velox.Edge{
		edge.To("stocks", Stock.Type).Annotations(graphql.Skip(graphql.SkipInputs)),
		edge.To("orders", Order.Type).Annotations(graphql.Skip(graphql.SkipInputs)),
	}
}

func (Warehouse) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}

// Stock is how many of one product one warehouse holds. createStock opens
// the row; after that the count only moves by adjustStock, placeOrder and
// cancelOrder, none of which can take it below zero.
type Stock struct{ velox.Schema }

func (Stock) Fields() []velox.Field {
	return []velox.Field{
		field.Int("quantity").NonNegative(),
	}
}

func (Stock) Edges() []velox.Edge {
	return []velox.Edge{
		edge.From("warehouse", Warehouse.Type).Ref("stocks").Unique().Required(),
		edge.From("product", Product.Type).Ref("stocks").Unique().Required(),
	}
}

// Indexes makes (warehouse, product) one row. Without it two rows could
// hold the same product, and taking stock would pick one of them.
func (Stock) Indexes() []velox.Index {
	return []velox.Index{
		index.Edges("warehouse", "product").Unique(),
	}
}

func (Stock) Annotations() []schema.Annotation {
	return []schema.Annotation{
		// An explicit QueryField keeps stocks a plain list, and every edge to
		// Stock with it; velox otherwise makes each one a connection.
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate()),
	}
}
