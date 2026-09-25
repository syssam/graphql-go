package schema

import (
	"github.com/syssam/velox"
	"github.com/syssam/velox/contrib/graphql"
	"github.com/syssam/velox/schema"
	"github.com/syssam/velox/schema/edge"
	"github.com/syssam/velox/schema/field"
)

type Warehouse struct{ velox.Schema }

func (Warehouse) Fields() []velox.Field {
	return []velox.Field{
		field.String("name").NotEmpty().Unique(),
	}
}

func (Warehouse) Edges() []velox.Edge {
	return []velox.Edge{
		edge.To("stocks", Stock.Type),
	}
}

func (Warehouse) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate()),
	}
}

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

func (Stock) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}
