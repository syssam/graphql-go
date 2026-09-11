package graphql

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
)

// Source supplies SDL to NewSchema.
type Source struct {
	files    []*ast.Source
	fsys     fs.FS
	patterns []string
}

// SDL returns a Source holding one SDL document.
func SDL(s string) Source {
	return Source{files: []*ast.Source{{Name: "schema.graphql", Input: s}}}
}

// SDLBytes returns a Source holding one SDL document.
func SDLBytes(b []byte) Source {
	return SDL(string(b))
}

// SDLFS returns a Source that reads every file in fsys matching one of the
// glob patterns. Files are read when NewSchema runs and keep their names for
// error positions.
func SDLFS(fsys fs.FS, patterns ...string) Source {
	return Source{fsys: fsys, patterns: patterns}
}

// Sources concatenates several sources.
func Sources(srcs ...Source) Source {
	var out Source
	for _, s := range srcs {
		out.files = append(out.files, s.files...)
		if s.fsys != nil {
			if out.fsys != nil && out.fsys != s.fsys {
				panic("graphql: Sources cannot combine more than one fs.FS; call SDLFS once with all patterns")
			}
			out.fsys = s.fsys
			out.patterns = append(out.patterns, s.patterns...)
		}
	}
	return out
}

func (s Source) load() ([]*ast.Source, error) {
	out := append([]*ast.Source(nil), s.files...)
	if s.fsys != nil {
		for _, pattern := range s.patterns {
			matches, err := fs.Glob(s.fsys, pattern)
			if err != nil {
				return nil, fmt.Errorf("graphql: glob %q: %w", pattern, err)
			}
			for _, name := range matches {
				b, err := fs.ReadFile(s.fsys, name)
				if err != nil {
					return nil, fmt.Errorf("graphql: read %s: %w", name, err)
				}
				out = append(out, &ast.Source{Name: name, Input: string(b)})
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("graphql: no SDL sources provided")
	}
	return out, nil
}

// SchemaOption configures a Schema. All binding constructors return one.
type SchemaOption interface {
	applySchema(*schemaBuilder)
}

type schemaOptionFunc func(*schemaBuilder)

func (f schemaOptionFunc) applySchema(b *schemaBuilder) { f(b) }

// DisableIntrospection rejects operations that select __schema or __type.
// __typename remains available.
func DisableIntrospection() SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) { b.introspection = false })
}

// Schema is a validated GraphQL schema with all bindings resolved. It is
// immutable and safe for concurrent use.
type Schema struct {
	ast           *ast.Schema
	reg           *registry
	objects       map[string]*objectType
	goTypes       map[reflect.Type]*objectType
	abstracts     map[string]*abstractType
	introspection bool

	query, mutation, subscription *objectType
}

// AST returns the parsed schema definition.
func (s *Schema) AST() *ast.Schema { return s.ast }

// IntrospectionEnabled reports whether __schema and __type may be queried.
func (s *Schema) IntrospectionEnabled() bool { return s.introspection }

// PrintSDL renders the schema as SDL, excluding built-in types and
// directives.
func PrintSDL(s *Schema) string {
	var sb strings.Builder
	formatter.NewFormatter(&sb).FormatSchema(s.ast)
	return sb.String()
}

// objectType is a resolved GraphQL object type.
type objectType struct {
	name           string
	def            *ast.Definition
	shapes         objectShapes
	isRoot         bool
	fields         map[string]*fieldDef
	hasSchedulable bool
}

// schemaBuilder accumulates bindings while options are applied and resolves
// them once every registration is known, so option order never matters.
type schemaBuilder struct {
	ast           *ast.Schema
	reg           *registry
	objects       map[string]*objectBinding
	objectOrder   []string
	inputs        []*inputBinding
	abstracts     map[string]*abstractBinding
	directives    map[string]*directiveBinding
	introspection bool
	errs          []error
}

func (b *schemaBuilder) errorf(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf("graphql: "+format, args...))
}

// NewSchema parses the SDL, applies every binding option and validates that
// the bindings cover the schema with compatible Go shapes. All problems are
// reported together in the returned error.
func NewSchema(src Source, opts ...SchemaOption) (*Schema, error) {
	sources, err := src.load()
	if err != nil {
		return nil, err
	}
	parsed, err := gqlparser.LoadSchema(sources...)
	if err != nil {
		return nil, fmt.Errorf("graphql: load schema: %w", err)
	}

	b := &schemaBuilder{
		ast:           parsed,
		reg:           newRegistry(),
		objects:       make(map[string]*objectBinding),
		abstracts:     make(map[string]*abstractBinding),
		directives:    make(map[string]*directiveBinding),
		introspection: true,
	}
	registerBuiltins(b)
	for _, opt := range opts {
		opt.applySchema(b)
	}
	if b.introspection {
		for _, opt := range introspectionOptions(b) {
			opt.applySchema(b)
		}
	}

	s := b.build()
	if len(b.errs) > 0 {
		return nil, errors.Join(b.errs...)
	}
	return s, nil
}

func (b *schemaBuilder) build() *Schema {
	s := &Schema{
		ast:           b.ast,
		reg:           b.reg,
		objects:       make(map[string]*objectType, len(b.objects)),
		goTypes:       make(map[reflect.Type]*objectType, len(b.objects)*2),
		abstracts:     make(map[string]*abstractType),
		introspection: b.introspection,
	}

	// Phase 1: object shells, so that shapes and Go-type lookups exist before
	// any field is composed.
	for _, name := range b.objectOrder {
		ob := b.objects[name]
		def := b.ast.Types[name]
		if def == nil {
			b.errorf("Object %q: type is not defined in the schema", name)
			continue
		}
		if def.Kind != ast.Object {
			b.errorf("Object %q: %s is %s, not an object type", name, name, strings.ToLower(string(def.Kind)))
			continue
		}
		obj := &objectType{name: name, def: def, shapes: ob.shapes, fields: make(map[string]*fieldDef, len(def.Fields))}
		obj.isRoot = isRootType(b.ast, def)
		if obj.isRoot && ob.shapes.elem != reflect.TypeFor[Root]() {
			b.errorf("Object %q: root operation types must be bound to graphql.Root, got %s", name, ob.shapes.elem)
			continue
		}
		s.objects[name] = obj
		if !obj.isRoot {
			if prev, dup := s.goTypes[ob.shapes.elem]; dup {
				b.errorf("Object %q: Go type %s is already bound to %s", name, ob.shapes.elem, prev.name)
				continue
			}
			s.goTypes[ob.shapes.elem] = obj
			s.goTypes[ob.shapes.ptr] = obj
		}
	}

	// Phase 2: input decoders, whose setters may reference any input type.
	for _, ib := range b.inputs {
		ib.resolve(b)
	}

	// Phase 3: abstract types, so that fields returning them can be checked.
	for name, def := range b.ast.Types {
		if !def.IsAbstractType() {
			continue
		}
		s.abstracts[name] = b.resolveAbstract(s, name, def)
	}

	// Phase 4: fields.
	for _, name := range b.objectOrder {
		obj := s.objects[name]
		if obj == nil {
			continue
		}
		b.resolveFields(s, obj, b.objects[name])
	}

	// Phase 5: schema directives wrap the composed executors.
	b.applyDirectives(s)

	// Phase 6: coverage.
	b.validateCoverage(s)

	if b.ast.Query != nil {
		s.query = s.objects[b.ast.Query.Name]
	}
	if b.ast.Mutation != nil {
		s.mutation = s.objects[b.ast.Mutation.Name]
	}
	if b.ast.Subscription != nil {
		s.subscription = s.objects[b.ast.Subscription.Name]
	}
	return s
}

func isRootType(s *ast.Schema, def *ast.Definition) bool {
	return (s.Query != nil && s.Query == def) ||
		(s.Mutation != nil && s.Mutation == def) ||
		(s.Subscription != nil && s.Subscription == def)
}

// validateCoverage ensures that every object type in the schema is bound and
// that every field of a bound type has an executor.
func (b *schemaBuilder) validateCoverage(s *Schema) {
	for name, def := range b.ast.Types {
		if def.Kind != ast.Object || def.BuiltIn {
			continue
		}
		obj := s.objects[name]
		if obj == nil {
			if _, attempted := b.objects[name]; attempted {
				continue
			}
			b.errorf("type %s has no Object binding", name)
			continue
		}
		attempted := b.objects[name].attempted
		for _, fd := range def.Fields {
			if strings.HasPrefix(fd.Name, "__") || attempted[fd.Name] {
				continue
			}
			if _, ok := obj.fields[fd.Name]; !ok {
				b.errorf("field %s has no binding", coordinate(name, fd.Name))
			}
		}
	}
}

// isLeaf reports whether a named SDL type is a scalar or an enum.
func isLeaf(s *ast.Schema, t *ast.Type) bool {
	def := s.Types[t.Name()]
	return def != nil && def.IsLeafType()
}

// listDepth counts the list levels of an SDL type.
func listDepth(t *ast.Type) int {
	n := 0
	for t.Elem != nil {
		n++
		t = t.Elem
	}
	return n
}

// coordinate renders a schema coordinate such as User.posts.
func coordinate(typeName, field string) string {
	return typeName + "." + field
}
