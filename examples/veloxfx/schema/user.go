// Package schema declares the entities velox generates the ORM and the SDL
// from. It is the only source of either: velox/ and graph/ are both output.
package schema

import (
	"github.com/syssam/velox"
	"github.com/syssam/velox/contrib/graphql"
	"github.com/syssam/velox/schema"
	"github.com/syssam/velox/schema/edge"
	"github.com/syssam/velox/schema/field"
)

// User owns todos.
type User struct{ velox.Schema }

func (User) Fields() []velox.Field {
	return []velox.Field{
		field.String("name").NotEmpty(),
		field.String("email").Unique(),
	}
}

func (User) Edges() []velox.Edge {
	return []velox.Edge{
		edge.To("todos", Todo.Type),
	}
}

func (User) Annotations() []schema.Annotation {
	return []schema.Annotation{
		graphql.QueryField(),
		graphql.Mutations(graphql.MutationCreate()),
	}
}
