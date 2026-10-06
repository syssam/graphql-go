package codegen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/fedlink"
)

const rootPkgDoc = "wires the generated GraphQL schema and its bindings."

func (b *builder) emitSchema() string {
	if b.cfg.Federation {
		return b.emitSchemaFederated()
	}
	var w strings.Builder
	w.WriteString(b.header(b.pkgName, rootPkgDoc))
	w.WriteString(`import (
	"embed"

	"github.com/syssam/graphql-go"
)

` + b.emitEmbed() + `
func NewSchema(r Resolver, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	all := append([]graphql.SchemaOption{Bindings(r)}, opts...)
	return graphql.NewSchema(graphql.SDLFS(sdl, "schema/*.graphql"), all...)
}

// ValidateSchema builds the schema with no resolver behind it, so every
// binding error surfaces before a resolver is written. Building never calls a
// resolver -- Resolve captures it in a closure -- so the zero value is enough.
func ValidateSchema(opts ...graphql.SchemaOption) error {
	var r Resolver
	_, err := NewSchema(r, opts...)
	return err
}
`)
	return w.String()
}

// emitSchemaFederated is emitSchema for Config.Federation: the schema is a
// subgraph, and NewSchema takes the entity resolvers.
func (b *builder) emitSchemaFederated() string {
	var w strings.Builder
	w.WriteString(b.header(b.pkgName, rootPkgDoc))
	w.WriteString(`import (
	"context"
	"embed"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/fed"
)

` + b.emitEmbed() + `
// NewSchema builds the subgraph: entities resolves each @key type the router
// reaches through _entities, and _service returns the embedded SDL.
func NewSchema(r Resolver, entities []fed.Entity, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	src, subgraph, err := fed.SubgraphFS(sdl, []string{"schema/*.graphql"}, entities...)
	if err != nil {
		return nil, err
	}
	all := append([]graphql.SchemaOption{subgraph, Bindings(r)}, opts...)
	return graphql.NewSchema(src, all...)
}

// ValidateSchema builds the schema with no resolver behind it, so every
// binding error surfaces before a resolver is written. Building never calls a
// resolver -- Resolve captures it in a closure -- so the zero value is enough.
func ValidateSchema(opts ...graphql.SchemaOption) error {
	var r Resolver
	_, err := NewSchema(r, placeholderEntities(), opts...)
	return err
}
`)
	w.WriteString(b.emitPlaceholderEntities())
	return w.String()
}

// emitEmbed renders the embedded SDL: the variable, and above it the name of
// every copy this run writes into schema/, and the custom scalars it named.
// Neither is for the reader of schema.go so much as for the next run, which
// reads them back (recordedIn): the first to know which files in that
// directory are gqlc's own, the second to name only a scalar that is new.
func (b *builder) emitEmbed() string {
	var w strings.Builder
	w.WriteString("// " + copiesListed + "\n//\n")
	names := make([]string, len(b.sources))
	for i, src := range b.sources {
		names[i] = copyName(src.Name)
	}
	slices.Sort(names)
	for _, name := range names {
		w.WriteString("//\t" + name + "\n")
	}
	w.WriteString(`//
// The next generate goes by this list. A copy on it that no source produces
// any more is deleted; a file in schema/ that is not on it is not gqlc's, and
// is refused instead of being deleted or left embedded.
`)
	if len(b.unboundScalars) > 0 {
		// One line, which no reader of the list above takes for a copy.
		w.WriteString("//\n// " + scalarsListed + " " + strings.Join(b.unboundScalars, " ") + `
//
// The next generate goes by that too, and names only a scalar that is not
// there: one is bound once, and being told so on every run buried the one
// that was new.
`)
	}
	w.WriteString(`//
//go:embed schema/*.graphql
var sdl embed.FS

`)
	return w.String()
}

// groupedNewSchemaDoc is written above the grouped NewSchema. Each group is
// registered the way a gRPC server registers a service, rather than through a
// struct with a field per group: a field left unset compiled, built, and
// failed on the first request to reach it, where a group left unregistered
// fails NewSchema with the fields it leaves unbound.
const groupedNewSchemaDoc = `// NewSchema builds the schema from the embedded SDL and the bindings given.
// Pass each group's bindings over its Resolver, as a gRPC server registers
// each service:
//
//	graph.NewSchema(user.Bindings(userResolver), post.Bindings(postResolver))
//
// A group that is not passed leaves its fields unbound, which NewSchema
// reports. Groups with no Resolver are included already.
`

func (b *builder) emitSchemaGrouped(groups []string) string {
	var w strings.Builder
	w.WriteString(b.header(b.pkgName, rootPkgDoc))
	if b.cfg.Federation {
		w.WriteString("import (\n\t\"context\"\n\t\"embed\"\n\n\t\"github.com/syssam/graphql-go\"\n\t\"github.com/syssam/graphql-go/fed\"\n")
	} else {
		w.WriteString("import (\n\t\"embed\"\n\n\t\"github.com/syssam/graphql-go\"\n")
	}
	for _, g := range groups {
		w.WriteString("\t\"" + b.groupImport(g) + "\"\n")
	}
	w.WriteString(")\n\n")
	w.WriteString(b.emitEmbed())
	w.WriteString(groupedNewSchemaDoc)
	if b.cfg.Federation {
		w.WriteString(federatedNewSchemaDoc)
		w.WriteString("func NewSchema(entities []fed.Entity, opts ...graphql.SchemaOption) (*graphql.Schema, error) {\n")
		w.WriteString("\tsrc, subgraph, err := fed.SubgraphFS(sdl, []string{\"schema/*.graphql\"}, entities...)\n")
		w.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		w.WriteString("\tall := []graphql.SchemaOption{\n\t\tsubgraph,\n")
	} else {
		w.WriteString("func NewSchema(opts ...graphql.SchemaOption) (*graphql.Schema, error) {\n")
		w.WriteString("\tall := []graphql.SchemaOption{\n")
	}
	for _, g := range groups {
		if !b.hasResolver(g) {
			w.WriteString("\t\t" + g + ".Bindings(),\n")
		}
	}
	w.WriteString("\t}\n")
	w.WriteString("\tall = append(all, opts...)\n")
	if b.cfg.Federation {
		w.WriteString("\treturn graphql.NewSchema(src, all...)\n}\n")
	} else {
		w.WriteString("\treturn graphql.NewSchema(graphql.SDLFS(sdl, \"schema/*.graphql\"), all...)\n}\n")
	}
	w.WriteString(`
// ValidateSchema builds the schema with no resolver behind it, so every
// binding error surfaces before a resolver is written. Building never calls a
// resolver -- Resolve captures it in a closure -- so a nil one is enough.
func ValidateSchema(opts ...graphql.SchemaOption) error {
	all := []graphql.SchemaOption{
`)
	for _, g := range groups {
		if b.hasResolver(g) {
			w.WriteString("\t\t" + g + ".Bindings(nil),\n")
		}
	}
	w.WriteString("\t}\n")
	if b.cfg.Federation {
		w.WriteString("\t_, err := NewSchema(placeholderEntities(), append(all, opts...)...)\n\treturn err\n}\n")
		w.WriteString(b.emitPlaceholderEntities())
	} else {
		w.WriteString("\t_, err := NewSchema(append(all, opts...)...)\n\treturn err\n}\n")
	}
	return w.String()
}

// federatedNewSchemaDoc follows groupedNewSchemaDoc when Config.Federation is
// set.
const federatedNewSchemaDoc = `//
// The schema is an Apollo Federation subgraph: entities resolves each @key
// type the router reaches through _entities, and _service returns the
// embedded SDL.
`

// keyedTypes returns the object types that are entities of this subgraph,
// sorted: the ones fed.Subgraph asks a resolver for, by the same reading.
func (b *builder) keyedTypes() []string {
	var names []string
	for name, def := range b.schema.Types {
		if def.Kind == ast.Object && !def.BuiltIn && fedlink.HasResolvableKey(def, b.fedNames) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// emitPlaceholderEntities writes a resolver for every @key type that is
// never called, so ValidateSchema can build a subgraph with no entity code
// written yet.
func (b *builder) emitPlaceholderEntities() string {
	var w strings.Builder
	w.WriteString("\n// placeholderEntities stands in for every @key type's resolver in\n// ValidateSchema, which builds the schema and resolves nothing.\nfunc placeholderEntities() []fed.Entity {\n\tnone := func(context.Context, fed.Representation) (*struct{}, error) { return nil, nil }\n\treturn []fed.Entity{\n")
	for _, name := range b.keyedTypes() {
		fmt.Fprintf(&w, "\t\tfed.Resolver(%q, none),\n", name)
	}
	w.WriteString("\t}\n}\n")
	return w.String()
}
