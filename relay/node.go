package relay

import (
	"context"

	graphql "github.com/syssam/graphql-go"
)

// NodeResolver returns the object a global id names, or nil when there is
// none. It receives the id already decoded, so an implementation switches on
// typeName rather than parsing anything.
//
// Returning (nil, nil) is "not found", which Query.node reports as null.
type NodeResolver func(ctx context.Context, typeName, id string) (any, error)

type nodeArgs struct{ ID graphql.ID }

// Node binds Query.node(id: ID!): Node, decoding the global id before fn
// sees it. The schema declares the Node interface and the field; this only
// binds them.
//
// The concrete object fn returns is matched to its GraphQL type the ordinary
// way, from its dynamic Go type, so the interface itself needs no binding.
func Node(fn NodeResolver) graphql.SchemaOption {
	return graphql.Options(
		graphql.Args[nodeArgs](graphql.InputField("id", func(a *nodeArgs, v graphql.ID) { a.ID = v })),
		graphql.Query(graphql.ResolveArgs("node", func(ctx context.Context, _ graphql.Root, a nodeArgs) (any, error) {
			typeName, id, err := FromGlobalID(a.ID)
			if err != nil {
				return nil, err
			}
			return fn(ctx, typeName, id)
		})),
	)
}

// IDField binds a type's id field to its global id. Binding it to the local
// id instead is the mistake that breaks the round trip: the id the client
// stores would not be one Query.node can resolve.
func IDField[E any](typeName string, local func(*E) string) graphql.FieldOption {
	return graphql.Field("id", func(e *E) graphql.ID { return ToGlobalID(typeName, local(e)) })
}
