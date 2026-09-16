// Package relay implements the Relay server contract for graphql-go: global
// object identification and cursor connections.
//
// SDL stays the contract. Nothing here emits types — the schema declares its
// Node interface, connections and edges as usual, and this package binds Go
// values to them. That is also what graphql-relay-js and graphql-java do;
// neither writes the user's schema for them.
//
// A connection needs three calls: Pagination once per schema, Bind once per
// connection, and FromSlice or FromPage in the resolver.
//
//	graphql.NewSchema(graphql.SDL(sdl),
//		relay.Pagination(),
//		relay.Bind[*User]("UserConnection", "UserEdge"),
//		graphql.Query(graphql.ResolveArgs("users",
//			func(ctx context.Context, _ graphql.Root, a relay.Args) (*relay.Connection[*User], error) {
//				c, err := relay.FromSlice(users, a)
//				return &c, err
//			})),
//	)
package relay
