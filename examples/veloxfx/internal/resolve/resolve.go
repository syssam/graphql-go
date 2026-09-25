// Package resolve is what every entity's resolver package shares: how its
// bindings reach the schema, and the conversions between velox's rows and the
// wire.
//
// Registration works the way a gRPC server registers services. Each entity
// package provides its own bindings into an fx value group, and the schema
// collects whatever is there. Adding an entity means adding a package and one
// line in its Module; nothing central lists every entity, so nothing central
// grows with the schema.
package resolve

import (
	"fmt"
	"strconv"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
)

// Bindings registers a constructor returning one group's bindings, typically
// func(r *Resolver) graphql.SchemaOption { return gen.Bindings(r) }.
//
// Forgetting it for a group is not silent: NewSchema reports every type the
// group leaves unbound, and fx fails to start.
func Bindings(constructor any) fx.Option {
	return fx.Provide(fx.Annotate(constructor, fx.ResultTags(`group:"graphql"`)))
}

// Registered is every group's bindings, as the schema constructor takes them.
type Registered struct {
	fx.In
	Bindings []graphql.SchemaOption `group:"graphql"`
}

// ID writes velox's int key as a GraphQL ID. It is a resolver, not a field
// binding, because AutoBind converts only within one basic kind.
func ID(id int) graphql.ID { return graphql.ID(strconv.Itoa(id)) }

// ParseID reads an ID argument back into velox's key.
func ParseID(id graphql.ID) (int, error) {
	n, err := strconv.Atoi(string(id))
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", id)
	}
	return n, nil
}

// List turns velox's nil for "no rows" into an empty list. The engine writes
// a nil slice as null, which a [T!]! field refuses.
func List[T any](rows []T, err error) ([]T, error) {
	if rows == nil && err == nil {
		rows = []T{}
	}
	return rows, err
}
