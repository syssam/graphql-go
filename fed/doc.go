// Package fed makes a schema an Apollo Federation subgraph: the _service and
// _entities root fields, the federation directives, and the _Entity union.
//
// It needs no change to the engine. _entities is an ordinary root field
// returning a union, and a union resolves its concrete type from the dynamic
// Go type of the value, which is the same path Query.node takes in the relay
// package.
//
// The author's SDL is the contract and is never rewritten. What the prelude
// adds is protocol scaffolding the author does not write in any
// implementation — the federation directive declarations, _Any, _Service, the
// _Entity union derived from the @key types, and the two root fields — which
// is how Apollo Server, graphql-java and gqlgen all do it. _service returns
// the author's text verbatim, because that text is what the router composes
// from.
//
//	src, bindings, err := fed.Subgraph(sdl,
//		fed.Resolver("User", func(ctx context.Context, r fed.Representation) (*User, error) {
//			id, _ := r.ID("id")
//			return loadUser(ctx, string(id))
//		}),
//	)
//	if err != nil {
//		return err
//	}
//	s, err := graphql.NewSchema(src, bindings, graphql.Object[User]("User", ...))
//
// A Federation 2 schema chooses how it writes the directives, and Subgraph
// reads all three ways from the schema's @link: imported and bare (@key),
// under the link's namespace when not imported (@federation__key, or
// @fed__key under as: "fed"), and renamed by an import
// ({name: "@key", as: "@entity"}). The constant Directives declares only the
// bare names.
//
// A @key is checked when the subgraph is built: every field it selects must
// exist, a field of an object, interface or union type must say which of its
// own fields are the key, and a field set carries no alias, argument or
// directive. Composition refuses each of those; the build says so first.
//
// A datastore lookup belongs in fed.BatchResolver, which receives every
// representation of its type at once; fed.Resolver resolves them one by one.
//
// Building the router that composes subgraphs is not in scope and is a much
// larger problem than serving one.
package fed
