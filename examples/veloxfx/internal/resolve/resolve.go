// Package resolve is what every domain package shares: how its groups'
// bindings reach the schema, and the conversions between velox's rows and the
// wire.
//
// Registration works the way a gRPC server registers services. Each domain
// provides its groups' bindings into an fx value group, and the schema
// collects whatever is there. Adding an entity means a Resolver type and one
// line in its domain's Module; nothing central lists every entity, so
// nothing central grows with the schema.
//
// Three things velox does that every resolver has to answer for:
//
//   - A query with no rows returns nil, which the engine writes as null and
//     a [T!]! field refuses. List turns it into an empty list.
//   - An edge method (Order.Customer) queries once per row unless the edge
//     was loaded, so the root resolvers eager-load what their type exposes.
//   - Create marks each required edge loaded with a stub holding only the id
//     (&entity.Customer{ID: id}), so returning what Save returned answers
//     `customer { name }` with an empty name. Every create reads its row back.
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
