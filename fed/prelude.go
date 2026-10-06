package fed

import (
	"strings"
)

// Directives declares the federation v2 directives a subgraph's SDL uses:
// @key, @external, @requires, @provides, @shareable, @inaccessible,
// @override, @tag, @interfaceObject, @composeDirective and @link. A tool that
// parses a subgraph's SDL on its own -- a code generator, a linter -- parses
// it after these, or @key is an undeclared directive.
//
// The field-set arguments are String rather than the specification's
// _FieldSet, and @link's for and import are String and [String] rather than
// link__Purpose and link__Import. The declarations exist only so the parser
// accepts the author's directives: the router never reads them, because what
// the router composes from is the author's own text returned by _service. A
// stricter prelude would only add scalar and enum types that every subgraph
// would then have to bind for coverage validation, with nothing reading them.
const Directives = `
directive @key(fields: String!, resolvable: Boolean = true) repeatable on OBJECT | INTERFACE
directive @external on OBJECT | FIELD_DEFINITION
directive @requires(fields: String!) on FIELD_DEFINITION
directive @provides(fields: String!) on FIELD_DEFINITION
directive @shareable repeatable on OBJECT | FIELD_DEFINITION
directive @inaccessible on FIELD_DEFINITION | OBJECT | INTERFACE | UNION | ARGUMENT_DEFINITION | SCALAR | ENUM | ENUM_VALUE | INPUT_OBJECT | INPUT_FIELD_DEFINITION
directive @override(from: String!, label: String) on FIELD_DEFINITION
directive @tag(name: String!) repeatable on FIELD_DEFINITION | OBJECT | INTERFACE | UNION | ARGUMENT_DEFINITION | SCALAR | ENUM | ENUM_VALUE | INPUT_OBJECT | INPUT_FIELD_DEFINITION | SCHEMA
directive @interfaceObject on OBJECT
directive @composeDirective(name: String!) repeatable on SCHEMA
directive @link(url: String!, as: String, for: String, import: [String]) repeatable on SCHEMA
`

// protocolFields is every field the prelude adds to a subgraph's schema, as
// graphql.RequireAuthCoverage names them. Coverage refuses a field that
// carries neither a requirement nor @public, and the author can say neither
// about these: they are not in the author's text. Marking them @public
// worked only in a subgraph that declared that directive for fields, so they
// are exempted from the check instead (bindings), which lifts nothing: a
// requirement on the Query type still guards them, as it guards any root
// field, and that is how a subgraph keeps _entities to its router.
var protocolFields = []string{"Query._service", "Query._entities", "_Service.sdl"}

// prelude returns Directives, the types the protocol fields use, and the
// machinery that depends on which types are entities. A subgraph with none
// declares neither _Entity nor _entities: an empty union is not a valid type,
// and _entities returning it would be a field the router must never call.
func prelude(entityNames []string) string {
	var b strings.Builder
	b.WriteString("\nscalar _Any\n\ntype _Service {\n  sdl: String!\n}\n")
	b.WriteString(Directives)
	if len(entityNames) == 0 {
		b.WriteString("\nextend type Query {\n  _service: _Service!\n}\n")
		return b.String()
	}
	b.WriteString("\nunion _Entity = ")
	b.WriteString(strings.Join(entityNames, " | "))
	b.WriteString("\n\nextend type Query {\n  _service: _Service!\n  _entities(representations: [_Any!]!): [_Entity]!\n}\n")
	return b.String()
}
