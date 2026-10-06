package fed

import (
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
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

// coverageDirective is the directive graphql.RequireAuthCoverage reads as
// "deliberately reachable by anyone".
const coverageDirective = "public"

// coverageMark is what the prelude writes after each field it adds so that
// graphql.RequireAuthCoverage accepts them: " @public" when the author's SDL
// declares that directive for field definitions, and nothing otherwise.
//
// Coverage refuses a field that carries neither a requirement nor @public,
// and these fields are not in the author's text, so the author can say
// neither about them. Left unanswered, the only way to build a covered
// subgraph is @public on the Query type, which exempts every root field the
// author did write. The mark is the true answer -- the router reaches them
// with no principal of its own -- and it is only an answer to coverage: a
// requirement on the Query type still guards them, as it guards any root
// field, which is how a subgraph keeps _entities to its router.
//
// It is written only where the author declared the directive, because a use
// of an undeclared directive does not load, and most subgraphs neither ask
// for coverage nor declare it.
func coverageMark(doc *ast.SchemaDocument) string {
	def := doc.Directives.ForName(coverageDirective)
	if def == nil || !slices.Contains(def.Locations, ast.LocationFieldDefinition) {
		return ""
	}
	return " @" + coverageDirective
}

// prelude returns Directives, the types the protocol fields use, and the
// machinery that depends on which types are entities. A subgraph with none
// declares neither _Entity nor _entities: an empty union is not a valid type,
// and _entities returning it would be a field the router must never call.
// mark follows each field the prelude adds; see coverageMark.
func prelude(entityNames []string, mark string) string {
	var b strings.Builder
	b.WriteString("\nscalar _Any\n\ntype _Service {\n  sdl: String!" + mark + "\n}\n")
	b.WriteString(Directives)
	if len(entityNames) == 0 {
		b.WriteString("\nextend type Query {\n  _service: _Service!" + mark + "\n}\n")
		return b.String()
	}
	b.WriteString("\nunion _Entity = ")
	b.WriteString(strings.Join(entityNames, " | "))
	b.WriteString("\n\nextend type Query {\n  _service: _Service!" + mark +
		"\n  _entities(representations: [_Any!]!): [_Entity]!" + mark + "\n}\n")
	return b.String()
}
