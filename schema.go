package graphql

import (
	"fmt"
	"github.com/syssam/graphql-go/internal/bindhook"
	"reflect"
	"slices"
	"strings"

	"github.com/syssam/graphql-go/internal/sdlprint"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// SchemaOption configures a Schema. All binding constructors return one.
type SchemaOption interface {
	applySchema(*schemaBuilder)
}

type schemaOptionFunc func(*schemaBuilder)

func (f schemaOptionFunc) applySchema(b *schemaBuilder) { f(b) }

// Options combines schema options into one. Generated Bindings use it so a
// group can return a single SchemaOption.
func Options(opts ...SchemaOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		for _, o := range opts {
			if o != nil {
				o.applySchema(b)
			}
		}
	})
}

// DisableIntrospection rejects operations that select __schema or __type.
// __typename remains available.
func DisableIntrospection() SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) { b.introspection = false })
}

// RequireAuthCoverage fails NewSchema for any field that declares neither an
// authorization requirement nor @public. It is opt-in because it breaks every
// schema that has not adopted it.
//
// The failure it prevents is not a field someone forgot to guard but a type
// nobody noticed arriving: a generated aggregate surface keyed by a resource
// no role grants and no role restricts is unguarded in both directions.
func RequireAuthCoverage() SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) { b.authCoverage = true })
}

// Query binds fields of the schema's query root, which is named Query
// unless the schema declaration says otherwise.
func Query(fields ...FieldOption) SchemaOption {
	return rootObject(func(b *schemaBuilder) *ast.Definition { return b.ast.Query }, "Query", fields)
}

// Mutation binds fields of the schema's mutation root.
func Mutation(fields ...FieldOption) SchemaOption {
	return rootObject(func(b *schemaBuilder) *ast.Definition { return b.ast.Mutation }, "Mutation", fields)
}

// Subscription binds fields of the schema's subscription root.
func Subscription(fields ...FieldOption) SchemaOption {
	return rootObject(func(b *schemaBuilder) *ast.Definition { return b.ast.Subscription }, "Subscription", fields)
}

func rootObject(root func(*schemaBuilder) *ast.Definition, fallback string, fields []FieldOption) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		name := fallback
		if def := root(b); def != nil {
			name = def.Name
		}
		Object[Root](name, fields...).applySchema(b)
	})
}

// Schema is a validated GraphQL schema with all bindings resolved. It is
// immutable and safe for concurrent use.
type Schema struct {
	ast     *ast.Schema
	reg     *registry
	objects map[string]*objectType
	// goTypes maps a Go type to every object bound to it. The same Go
	// type may back several GraphQL objects (for example User and
	// UserSummary); abstract resolution then requires a TypeResolver
	// when more than one of those objects is a possible type.
	goTypes       map[reflect.Type][]*objectType
	abstracts     map[string]*abstractType
	inputs        map[string]*inputInfo
	introspection bool

	query, mutation, subscription *objectType
}

// AST returns the parsed schema definition.
func (s *Schema) AST() *ast.Schema { return s.ast }

// IntrospectionEnabled reports whether __schema and __type may be queried.
func (s *Schema) IntrospectionEnabled() bool { return s.introspection }

// PrintSDL renders the schema as SDL, excluding built-in types and
// directives.
//
// Every applied directive is printed, including ones the schema uses
// internally such as authorization requirements; a schema published from this
// output publishes those too. fed.Subgraph serves the author's own SDL instead.
//
// The output reloads as the same schema, descriptions and string values
// included, which gqlparser's formatter does not guarantee; see sdlprint.
func PrintSDL(s *Schema) string { return sdlprint.Print(s.ast) }

// objectType is a resolved GraphQL object type.
type objectType struct {
	name           string
	def            *ast.Definition
	shapes         objectShapes
	isRoot         bool
	fields         map[string]*fieldDef
	hasSchedulable bool

	// requires is the object's effective type-level requirement (its own
	// AND its interfaces'), which guards __typename on this type.
	requires Requirement

	// instanceGuarded reports that the type carries @authorizeObject, so a
	// value of it is offered to the ObjectAuthorizer before being written.
	instanceGuarded bool
}

// schemaBuilder accumulates bindings while options are applied and resolves
// them once every registration is known, so option order never matters.
type schemaBuilder struct {
	ast         *ast.Schema
	reg         *registry
	objects     map[string]*objectBinding
	objectOrder []string
	inputs      []*inputBinding
	abstracts   map[string]*abstractBinding
	directives  map[string]*directiveBinding
	// subDirectives names directives the application enforces itself when a subscription
	// opens; see SubscriptionRootDirective.
	subDirectives map[string]bool
	// boundChecks are objects another package of this module needs bound to
	// a particular Go type; see internal/bindhook.
	boundChecks []boundCheck
	// leafBound is every (leaf type, Go type) an option has bound, so a
	// second binding of the pair is refused rather than replacing the first.
	leafBound     map[typeKey]bool
	introspection bool
	authCoverage  bool
	authCapped    map[string]bool
	reqDirectives []reqDirective
	errs          []error
	// phaseStart is where the current build phase began in errs, so endPhase
	// sorts only what this phase added.
	phaseStart int
}

func (b *schemaBuilder) errorf(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf("graphql: "+format, args...))
}

// endPhase orders the errors this phase produced.
//
// Errors stay in phase order across phases, because the earlier one is often
// the cause of the later: an Object that named a type the schema does not
// declare, before every field of that type reporting itself unbound. Within a
// phase there is no such relationship and several of the checks range over
// b.ast.Types, which is a map -- so a schema with 40 unbound types printed
// them in a different order on every run, and one with thousands made two runs
// impossible to diff. Sorting by message also puts a category together, since
// each begins with its own wording.
func (b *schemaBuilder) endPhase() {
	rest := b.errs[b.phaseStart:]
	slices.SortFunc(rest, func(x, y error) int { return strings.Compare(x.Error(), y.Error()) })
	b.phaseStart = len(b.errs)
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
	patchPrelude(parsed)

	b := &schemaBuilder{
		ast:           parsed,
		reg:           newRegistry(),
		objects:       make(map[string]*objectBinding),
		abstracts:     make(map[string]*abstractBinding),
		directives:    make(map[string]*directiveBinding),
		subDirectives: make(map[string]bool),
		leafBound:     make(map[typeKey]bool),
		introspection: true,
		// Seeded before options apply, so the Apollo spelling is always present
		// and a caller redeclaring it collides rather than silently winning.
		reqDirectives: []reqDirective{{name: authDirective, arg: "scopes", shape: ScopesNested, builtin: true}},
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
		return nil, &buildError{errs: b.errs}
	}
	return s, nil
}

func (b *schemaBuilder) build() *Schema {
	s := &Schema{
		ast:           b.ast,
		reg:           b.reg,
		objects:       make(map[string]*objectType, len(b.objects)),
		goTypes:       make(map[reflect.Type][]*objectType, len(b.objects)*2),
		abstracts:     make(map[string]*abstractType),
		inputs:        indexInputs(b.ast),
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
		obj.instanceGuarded = def.Directives.ForName(objectDirective) != nil
		if obj.isRoot && ob.shapes.elem != reflect.TypeFor[Root]() {
			b.errorf("Object %q: root operation types must be bound to graphql.Root, got %s", name, ob.shapes.elem)
			continue
		}
		s.objects[name] = obj
		if !obj.isRoot {
			s.goTypes[ob.shapes.elem] = append(s.goTypes[ob.shapes.elem], obj)
			s.goTypes[ob.shapes.ptr] = append(s.goTypes[ob.shapes.ptr], obj)
		}
	}

	for _, c := range b.boundChecks {
		// An unbound or undefined object is reported where its binding is
		// missed; this is only about one bound to something else.
		if ob := b.objects[c.name]; ob != nil && ob.shapes.elem != c.goType {
			b.errorf("%s: it returns %s, but %s is bound to %s", c.what, c.goType, c.name, ob.shapes.elem)
		}
	}

	b.endPhase()

	// Phase 2: input decoders, whose setters may reference any input type.
	for _, ib := range b.inputs {
		ib.resolve(b)
	}

	b.endPhase()

	// Phase 3: abstract types, so that fields returning them can be checked.
	for name, def := range b.ast.Types {
		if !def.IsAbstractType() {
			continue
		}
		s.abstracts[name] = b.resolveAbstract(s, name, def)
	}

	b.endPhase()

	// Phase 4: fields.
	for _, name := range b.objectOrder {
		obj := s.objects[name]
		if obj == nil {
			continue
		}
		b.resolveFields(s, obj, b.objects[name])
	}

	b.endPhase()

	// Phase 5: schema directives wrap the composed executors.
	b.applyDirectives(s)

	b.endPhase()

	// Phase 6: coverage.
	b.validateCoverage(s)
	b.validateRequirementDirectives()
	b.validateAuthDirectives()
	b.validateInputDirectives()
	b.validateObjectDirectives()
	b.resolveAuthRequirements(s)
	b.validateAuthCoverage(s)
	b.endPhase()

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
	b.validateSubscriptionRoot(s)
	b.validateDeprecation()
}

// validateDeprecation enforces specification section 3.13.2: @deprecated must
// not appear on a required argument or input field. A client cannot stop
// supplying one, so the deprecation would be advice it is unable to take.
func (b *schemaBuilder) validateDeprecation() {
	deprecatedAndRequired := func(dirs ast.DirectiveList, t *ast.Type, def *ast.Value) bool {
		return dirs.ForName("deprecated") != nil && t.NonNull && def == nil
	}
	for _, def := range b.ast.Types {
		if def.BuiltIn {
			continue
		}
		switch def.Kind {
		case ast.Object, ast.Interface:
			for _, f := range def.Fields {
				for _, a := range f.Arguments {
					if deprecatedAndRequired(a.Directives, a.Type, a.DefaultValue) {
						b.errorf("argument %s(%s:) is required and cannot be @deprecated; make it nullable or give it a default first", coordinate(def.Name, f.Name), a.Name)
					}
				}
			}
		case ast.InputObject:
			for _, f := range def.Fields {
				if deprecatedAndRequired(f.Directives, f.Type, f.DefaultValue) {
					b.errorf("input field %s is required and cannot be @deprecated; make it nullable or give it a default first", coordinate(def.Name, f.Name))
				}
			}
		}
	}
	for _, d := range b.ast.Directives {
		for _, a := range d.Arguments {
			if deprecatedAndRequired(a.Directives, a.Type, a.DefaultValue) {
				b.errorf("argument @%s(%s:) is required and cannot be @deprecated; make it nullable or give it a default first", d.Name, a.Name)
			}
		}
	}
}

// validateSubscriptionRoot requires every subscription root field to carry a
// source stream. Field or Resolve there would compose cleanly and then have
// nothing to subscribe to, so the failure belongs at NewSchema.
func (b *schemaBuilder) validateSubscriptionRoot(s *Schema) {
	def := b.ast.Subscription
	if def == nil {
		return
	}
	obj := s.objects[def.Name]
	if obj == nil {
		return
	}
	for _, f := range def.Fields {
		fd, ok := obj.fields[f.Name]
		if ok && fd.subscribe == nil {
			b.errorf("field %s must be bound with Subscribe or SubscribeArgs, not Field or Resolve", coordinate(def.Name, f.Name))
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

// boundCheck is one internal/bindhook request.
type boundCheck struct {
	what, name string
	goType     reflect.Type
}

func init() {
	bindhook.ObjectBoundTo = func(what, name string, t reflect.Type) any {
		return schemaOptionFunc(func(b *schemaBuilder) {
			b.boundChecks = append(b.boundChecks, boundCheck{what: what, name: name, goType: t})
		})
	}
}
