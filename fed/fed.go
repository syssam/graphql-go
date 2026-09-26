package fed

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	graphql "github.com/syssam/graphql-go"
)

// Entity binds one entity type's resolver. Build one with Resolver or
// BatchResolver.
type Entity struct {
	typename string
	resolve  func(context.Context, Representation) (any, error)
	batch    func(context.Context, []Representation) ([]any, error)
}

// Resolver binds the resolver for the entity type named, the function the
// router reaches through _entities when another subgraph refers to it.
//
// Returning (nil, nil) is "no such entity", which _entities reports as null.
// An error fails the whole _entities field rather than one element, because a
// field resolver reports one error; a caller that wants the other entities to
// survive returns (nil, nil) for the one it cannot produce.
func Resolver[E any](typename string, fn func(context.Context, Representation) (*E, error)) Entity {
	return Entity{typename: typename, resolve: func(ctx context.Context, r Representation) (any, error) {
		v, err := fn(ctx, r)
		if err != nil {
			return nil, err
		}
		return v, nil
	}}
}

// BatchResolver binds a resolver that receives every representation of the
// entity type named in one _entities call, in request order, and returns one
// result for each, at the same index. It is the one to use for a datastore:
// a router sends all the representations it needs in a single call, so
// Resolver makes one lookup per representation, and a DataLoader inside it
// cannot coalesce them because they are resolved one after another.
//
// A nil element is "no such entity", reported as null. A result of the wrong
// length, or an error, fails the whole _entities field.
func BatchResolver[E any](typename string, fn func(context.Context, []Representation) ([]*E, error)) Entity {
	return Entity{typename: typename, batch: func(ctx context.Context, reps []Representation) ([]any, error) {
		vs, err := fn(ctx, reps)
		if err != nil {
			return nil, err
		}
		if len(vs) != len(reps) {
			return nil, fmt.Errorf("fed: %s batch resolver returned %d results for %d representations", typename, len(vs), len(reps))
		}
		out := make([]any, len(vs))
		for i, v := range vs {
			if v != nil {
				out[i] = v
			}
		}
		return out, nil
	}}
}

type service struct{ sdl string }

type entitiesArgs struct{ Representations []Representation }

// Subgraph returns the schema source and bindings for a federation subgraph.
//
// The source is the federation prelude followed by sdl; the bindings cover
// _Any, _Service, _Entity and the two root fields. The error reports a @key
// type with no resolver, or a resolver for a type that carries no @key —
// either is a subgraph that answers null to router fetches it is supposed to
// serve, and both are knowable before the server starts.
func Subgraph(sdl string, entities ...Entity) (graphql.Source, graphql.SchemaOption, error) {
	return subgraph(sdl, graphql.SDL(sdl), entities)
}

// SubgraphFS is Subgraph over SDL files, for a schema split across files or
// embedded by a code generator: every file in fsys matching one of the glob
// patterns, in path order. _service returns them joined in that order. The
// files keep their names for NewSchema's error messages.
func SubgraphFS(fsys fs.FS, patterns []string, entities ...Entity) (graphql.Source, graphql.SchemaOption, error) {
	seen := map[string]bool{}
	var names []string
	for _, p := range patterns {
		matches, err := fs.Glob(fsys, p)
		if err != nil {
			return graphql.Source{}, nil, fmt.Errorf("fed: %w", err)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				names = append(names, m)
			}
		}
	}
	if len(names) == 0 {
		return graphql.Source{}, nil, fmt.Errorf("fed: no SDL file matches %v", patterns)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return graphql.Source{}, nil, fmt.Errorf("fed: %w", err)
		}
		parts = append(parts, string(b))
	}
	return subgraph(strings.Join(parts, "\n"), graphql.SDLFS(fsys, patterns...), entities)
}

func subgraph(sdl string, author graphql.Source, entities []Entity) (graphql.Source, graphql.SchemaOption, error) {
	keyed, ifaces, err := keyTypes(sdl)
	if err != nil {
		return graphql.Source{}, nil, err
	}
	byName := make(map[string]Entity, len(entities))
	for _, e := range entities {
		if _, ok := ifaces[e.typename]; ok {
			return graphql.Source{}, nil, fmt.Errorf("fed: resolver for %q, an interface carrying @key: the router sends representations with a concrete __typename, so an entity interface is reached through its implementing types and has no resolver of its own", e.typename)
		}
		if _, ok := keyed[e.typename]; !ok {
			return graphql.Source{}, nil, fmt.Errorf("fed: resolver for %q, which carries no @key in this subgraph", e.typename)
		}
		if _, dup := byName[e.typename]; dup {
			return graphql.Source{}, nil, fmt.Errorf("fed: two resolvers for %q", e.typename)
		}
		byName[e.typename] = e
	}
	var missing []string
	for name := range keyed {
		if _, ok := byName[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return graphql.Source{}, nil, fmt.Errorf("fed: @key types with no resolver: %s", strings.Join(missing, ", "))
	}

	names := make([]string, 0, len(keyed))
	for name := range keyed {
		names = append(names, name)
	}
	slices.Sort(names)

	src := graphql.Sources(graphql.SDL(prelude(names)), author)
	return src, bindings(sdl, names, byName), nil
}

// keyTypes collects the types carrying @key, objects and interfaces apart.
// Only the objects are entities in the sense _Entity means: a union's members
// must be object types, and the router dispatches on a concrete __typename,
// so an interface carrying @key is reached through its implementing types
// rather than in its own right.
//
// The SDL is parsed rather than loaded: @key is not declared until the
// prelude is in front of it, and a loader would reject the document for that.
func keyTypes(sdl string) (objects, interfaces map[string]struct{}, err error) {
	doc, err := parser.ParseSchema(&ast.Source{Name: "subgraph.graphql", Input: sdl})
	if err != nil {
		return nil, nil, fmt.Errorf("fed: %w", err)
	}
	if err := checkKeys(doc); err != nil {
		return nil, nil, err
	}
	objects, interfaces = map[string]struct{}{}, map[string]struct{}{}
	collect := func(defs ast.DefinitionList) {
		for _, def := range defs {
			if def.Directives.ForName("key") == nil {
				continue
			}
			switch def.Kind {
			case ast.Object:
				objects[def.Name] = struct{}{}
			case ast.Interface:
				interfaces[def.Name] = struct{}{}
			}
		}
	}
	collect(doc.Definitions)
	collect(doc.Extensions)
	return objects, interfaces, nil
}

func bindings(sdl string, names []string, byName map[string]Entity) graphql.SchemaOption {
	opts := []graphql.SchemaOption{
		graphql.Scalar[Representation]("_Any",
			func(w *graphql.Writer, v Representation) error {
				b, err := json.Marshal(map[string]any(v))
				if err != nil {
					return err
				}
				w.Raw(b)
				return nil
			},
			func(v any) (Representation, error) {
				m, ok := v.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("_Any: expected an object, got %T", v)
				}
				return Representation(m), nil
			}),
		graphql.Object[service]("_Service",
			graphql.Field("sdl", func(s *service) string { return s.sdl }),
		),
	}
	root := []graphql.FieldOption{
		graphql.Resolve("_service", func(context.Context, graphql.Root) (*service, error) {
			return &service{sdl: sdl}, nil
		}),
	}
	if len(names) > 0 {
		opts = append(opts, graphql.Union[any]("_Entity"), graphql.Args[entitiesArgs]())
		root = append(root, graphql.ResolveArgs("_entities",
			func(ctx context.Context, _ graphql.Root, a entitiesArgs) ([]any, error) {
				return resolveEntities(ctx, byName, a.Representations)
			}))
	}
	opts = append(opts, graphql.Query(root...))
	return graphql.Options(opts...)
}

// resolveEntities answers each representation at its own index. A type bound
// with BatchResolver is gathered and called once; the rest are resolved one
// at a time.
func resolveEntities(ctx context.Context, byName map[string]Entity, reps []Representation) ([]any, error) {
	out := make([]any, len(reps))
	// order keeps the batch calls in first-appearance order, so which error a
	// failing request reports does not depend on map iteration.
	var batches map[string][]int
	var order []string
	for i, r := range reps {
		e, ok := byName[r.Typename()]
		if !ok {
			// Another subgraph owns it. Null is the answer, not an error: the
			// router asked every subgraph it might be in.
			continue
		}
		if e.batch != nil {
			if batches == nil {
				batches = map[string][]int{}
			}
			if _, seen := batches[e.typename]; !seen {
				order = append(order, e.typename)
			}
			batches[e.typename] = append(batches[e.typename], i)
			continue
		}
		v, err := e.resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	for _, name := range order {
		at := batches[name]
		group := make([]Representation, len(at))
		for j, i := range at {
			group[j] = reps[i]
		}
		vs, err := byName[name].batch(ctx, group)
		if err != nil {
			return nil, err
		}
		for j, i := range at {
			out[i] = vs[j]
		}
	}
	return out, nil
}
