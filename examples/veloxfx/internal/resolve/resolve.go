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
	"context"
	"errors"
	"fmt"
	"strconv"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
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

// Get is a by-id query: an id naming no row is a null result, not an error,
// because the field is nullable and the id simply named nothing.
func Get[T any](ctx context.Context, id graphql.ID, get func(context.Context, int) (*T, error)) (*T, error) {
	n, err := ParseID(id)
	if err != nil {
		return nil, err
	}
	v, err := get(ctx, n)
	if velox.IsNotFound(err) {
		return nil, nil
	}
	return v, err
}

// Delete is a delete by id, returning the id. A row something still refers
// to is refused by its foreign key, and that is reported as the reason
// rather than as the driver's error text.
func Delete(ctx context.Context, id graphql.ID, del func(context.Context, int) error) (graphql.ID, error) {
	n, err := ParseID(id)
	if err != nil {
		return "", err
	}
	if err := del(ctx, n); err != nil {
		if velox.IsNotFound(err) {
			return "", fmt.Errorf("%s: no such row", id)
		}
		if velox.IsConstraintError(err) {
			return "", fmt.Errorf("%s is still referenced; delete what refers to it first", id)
		}
		return "", err
	}
	return id, nil
}

// OrderField converts an order field gqlc modelled as a string enum into
// velox's. velox's order field is a struct holding a cursor func, which no
// GraphQL enum can bind to, and its values are unexported, so UnmarshalGQL
// is the one way to build one from outside the package.
func OrderField[F any, PF interface {
	*F
	UnmarshalGQL(any) error
}](name string) (*F, error) {
	f := PF(new(F))
	if err := f.UnmarshalGQL(name); err != nil {
		return nil, err
	}
	return f, nil
}

// InTx runs fn in one transaction: committed if fn returns no error, rolled
// back if it does, so a mutation that fails half-way leaves nothing behind.
// fn must use tx's clients, not the client it was given, or its writes
// happen outside the transaction.
func InTx[T any](ctx context.Context, c *velox.Client, fn func(tx *velox.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := c.Tx(ctx)
	if err != nil {
		return zero, err
	}
	v, err := fn(tx)
	if err != nil {
		return zero, errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return v, nil
}
