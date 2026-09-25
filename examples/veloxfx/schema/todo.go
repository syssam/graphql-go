package schema

import (
	"github.com/syssam/velox"
	"github.com/syssam/velox/contrib/graphql"
	"github.com/syssam/velox/schema"
	"github.com/syssam/velox/schema/edge"
	"github.com/syssam/velox/schema/field"
	"github.com/syssam/velox/schema/mixin"
)

// Todo belongs to exactly one user.
type Todo struct{ velox.Schema }

func (Todo) Mixin() []velox.Mixin {
	return []velox.Mixin{mixin.Time{}}
}

func (Todo) Fields() []velox.Field {
	return []velox.Field{
		field.String("title").NotEmpty(),
		field.Enum("status").Values("TODO", "DONE").Default("TODO"),
	}
}

func (Todo) Edges() []velox.Edge {
	return []velox.Edge{
		edge.From("owner", User.Type).Ref("todos").Unique().Required(),
	}
}

func (Todo) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate(), graphql.MutationUpdate()),
	}
}
