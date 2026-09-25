// Package schema declares the entities velox generates the ORM and the SDL
// from. It is the only source of either: velox/ and graph/ are both output.
//
// The entities fall into three domains, which is how internal/ is laid out:
//
//	catalog    Category, Product
//	sales      Customer, Order, OrderItem
//	inventory  Warehouse, Stock
//
// OrderItem.product and Stock.product cross from one domain into another.
package schema

import (
	"github.com/syssam/velox"
	"github.com/syssam/velox/contrib/graphql"
	"github.com/syssam/velox/schema"
	"github.com/syssam/velox/schema/edge"
	"github.com/syssam/velox/schema/field"
)

type Category struct{ velox.Schema }

func (Category) Fields() []velox.Field {
	return []velox.Field{
		field.String("name").NotEmpty().Unique(),
	}
}

func (Category) Edges() []velox.Edge {
	return []velox.Edge{
		edge.To("products", Product.Type),
	}
}

func (Category) Annotations() []schema.Annotation {
	return []schema.Annotation{
		// Filterable by name, which is also what lets products filter by
		// category: hasCategoryWith takes a CategoryWhereInput.
		graphql.WhereInputFields("name"),
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate()),
	}
}

type Product struct{ velox.Schema }

func (Product) Fields() []velox.Field {
	return []velox.Field{
		field.String("sku").NotEmpty().Unique(),
		field.String("name").NotEmpty().
			Annotations(graphql.OrderField("NAME")),
		field.Int("price_cents").NonNegative().
			Annotations(graphql.OrderField("PRICE")),
	}
}

func (Product) Edges() []velox.Edge {
	return []velox.Edge{
		edge.From("category", Category.Type).Ref("products").Unique().Required(),
		edge.To("stocks", Stock.Type),
		edge.To("order_items", OrderItem.Type),
	}
}

func (Product) Annotations() []schema.Annotation {
	return []schema.Annotation{
		// products(first, after, where, orderBy) is a Relay connection. Filtering
		// is opt-in per field: a column is not filterable until it is listed.
		graphql.RelayConnection(),
		graphql.WhereInputFields("sku", "name", "price_cents"),
		graphql.WhereInputEdges("category"),
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}
