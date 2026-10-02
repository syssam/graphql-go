package schema

import "github.com/syssam/velox/contrib/graphql"

// requiresScopes puts graphql-go's @requiresScopes on a field or edge: the
// caller needs every scope named. sdl/authz.graphql declares the directive.
func requiresScopes(scopes ...string) graphql.Annotation {
	return graphql.Directives(graphql.NewDirective("requiresScopes", map[string]any{
		"scopes": [][]string{scopes},
	}))
}

// key makes the type a federation entity keyed by fields: other subgraphs
// refer to it, and the router asks this one for it through _entities.
func key(fields string) graphql.Annotation {
	return graphql.Directives(graphql.NewDirective("key", map[string]any{"fields": fields}))
}
