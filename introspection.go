package graphql

import (
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator/core"
)

// noIntrospectionRule rejects __schema and __type selections when
// introspection is disabled. __typename remains allowed.
var noIntrospectionRule = core.Rule{
	Name: "NoIntrospection",
	RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		observers.OnField(func(_ *core.Walker, field *ast.Field) {
			if field.Name == "__schema" || field.Name == "__type" {
				addError(core.Message("GraphQL introspection is not allowed, but the query contained %s.", field.Name), core.At(field.Position))
			}
		})
	},
}

// introspectionOptions returns the bindings for the introspection schema.
// They are installed by NewSchema after user options.
func introspectionOptions() []SchemaOption {
	return nil
}
