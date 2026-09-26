package schema

import "github.com/syssam/velox/contrib/graphql"

// requiresScopes puts graphql-go's @requiresScopes on a field or edge: the
// caller needs every scope named. sdl/authz.graphql declares the directive.
func requiresScopes(scopes ...string) graphql.Annotation {
	return graphql.Directives(graphql.NewDirective("requiresScopes", map[string]any{
		"scopes": [][]string{scopes},
	}))
}
