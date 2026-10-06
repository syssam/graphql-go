package codegen

// Config drives Generate. A Manifest is not required: SDL types without a
// Models or Manifest entry get a generated model in Output/model.
//
// Experimental: a generator's configuration grows with the generator, and a
// field's meaning or default may change before v1; see docs/api-stability.md.
type Config struct {
	// Dir is the working directory for SchemaGlobs and Output. Empty means
	// the process working directory.
	Dir string
	// SchemaGlobs lists SDL files relative to Dir, as paths or globs. A path
	// element of ** is any number of directories, skipping those whose name
	// starts with a dot and Output's own schema directory; a glob that
	// matches nothing is an error. A source may have any extension: each is
	// copied into Output/schema under its own name with .graphql, and two
	// whose copies would share a name, letter case aside, are refused.
	SchemaGlobs []string
	// Output is the directory for generated Go, relative to Dir.
	Output string
	// Package is the import path of Output. Required so generated model
	// imports are correct.
	Package string
	// GroupFunc assigns a GraphQL type to a generated package. The
	// second argument is the SDL file that defined the type. A nil
	// function groups by the SDL file stem. Names schema, model and the
	// output package are remapped so they do not collide with embed or
	// model directories. One group stays flat in Output; two or more
	// become subpackages, each registered with NewSchema by its Bindings.
	GroupFunc func(typeName, sdlFile string) string
	// GroupDir is the directory, relative to Output, that holds one package
	// per group: "register" puts the user group's bindings and Resolver
	// interface in Output/register/user. Empty means Output itself. It lets
	// generated bindings, generated models (Output/model) and the resolvers
	// implementing them (Scaffold) each sit under one directory of their
	// own, while every group stays its own package -- which is what keeps a
	// one-group edit from recompiling the rest. With one group it has no
	// effect but is still checked, so the schema growing a second group is
	// not when a bad value is first reported.
	//
	// Changing it moves the generated packages, and the previous ones are
	// deleted; code you wrote that imports them keeps the old path, which the
	// compiler reports. Scaffold appends to such a file and never rewrites
	// what is there, imports included.
	//
	// Refused: a path leaving Output, the model or schema directory in any
	// letter case, a directory the go command ignores (testdata, vendor, a
	// name starting with "." or "_"), and one reaching into another gqlc run's
	// output root.
	GroupDir string
	// RootFieldGroup assigns one field of the query, mutation or subscription
	// root to a group. Empty, or a nil function, means the group GroupFunc
	// (or the file stem) gives the SDL file the field is declared in, so
	// `extend type Query { ... }` in user.graphql lands in the user group.
	//
	// Roots are grouped per field, not per type, because a root is the one
	// type every group adds to: grouped as a type, every root resolver of a
	// large schema ends up in one package, which is the monolith grouping is
	// there to split. A generator that declares every root field in one file
	// (velox does) can follow the returned type instead:
	//
	//	RootFieldGroup: func(f codegen.RootField) string { return f.ReturnGroup }
	RootFieldGroup func(RootField) string
	// Scaffold writes a stub for every Resolver method an implementation does
	// not have yet. It maps a group -- the output package name when there is
	// one group -- to the Go type implementing it, as "dir.Type" with dir
	// relative to Dir:
	//
	//	Scaffold: map[string]string{"product": "internal/catalog.ProductResolver"}
	//
	// The key "*" stands for every group with a Resolver that no other key
	// names, and its value says where the group's name goes with {group}:
	//
	//	Scaffold: map[string]string{"*": "graph/{group}.Handler"}
	//
	// so a schema that keeps one implementation per group in one layout
	// lists none of them, and a new SDL file needs no entry. A key that names
	// a group still wins for that group.
	//
	// Missing methods are appended to dir/<group>.resolvers.go, created with the
	// type and a var _ Resolver assertion if the type does not exist yet. Methods
	// that exist are never touched, wherever they are declared; the package is
	// parsed, not loaded. Empty means no scaffolding.
	//
	// dir may be the group's own generated package ("graph/product.Handler"),
	// so one entity is one package: the stubs then name the group's types
	// unqualified and do not import it. The author's code and the generated
	// code then share a namespace, so a Type the generator declares there,
	// such as Resolver, is refused, and so is a hand-written declaration of a
	// name the generator emits, naming the file it is in.
	Scaffold map[string]string
	// Models maps a GraphQL named type to a Go type expression
	// (for example Time → time.Time). Unmapped custom scalars become
	// named string types in the model package. An entry is a predeclared
	// type, or import/path.Type behind any * and [], the type being what
	// follows the last dot; a map or generic over another package's type is
	// refused, and a named type declared for it can be mapped instead.
	Models map[string]string
	// ModelDirective names an SDL directive that already carries a type's Go
	// binding, and the argument holding it. Empty means none.
	//
	// A schema arriving from another generator has those bindings written down
	// already -- gqlgen spells it @goModel(model: "pkg/path.Type") -- and one
	// real schema carries 3 778 of them. Restating those in Models is work with
	// no decision in it, and a translation step that has to be rerun whenever
	// the SDL changes is a second source of truth.
	//
	// The spelling is declared rather than hardcoded, for the reason
	// RequirementDirective gives in the root package: a generator that only
	// understands one vendor's directive name is a generator that has to be
	// forked to understand the next one. Models still wins where both name a
	// type, so a config can override what the SDL says.
	ModelDirective ModelDirective
	// FieldDirective names an SDL directive that forces a field to the
	// Resolver interface, and the boolean argument that turns it on. Empty
	// means none.
	//
	// gqlgen spells it @goField(forceResolver: true), and one real schema
	// carries 4 567 of them. It is not redundant with AutoBind refusing a
	// guess: AutoBind verifies that a struct field exists, and the author is
	// saying the value must be computed rather than read -- permission
	// filtering, a currency conversion, a field that is a column today and
	// will not be tomorrow. Binding it to the column anyway compiles and
	// answers the wrong thing, which is worse than not compiling.
	//
	// Other arguments gqlgen's directive takes (name:, omittable:) are not
	// read.
	//
	// Measured: on the 5 503-type schema it was built for it moved the resolver
	// count by exactly zero, because every field it forces was a computed field
	// the Go type does not have. It earns its place on the field that is a
	// column and that the author wants computed anyway; do not quote it as a
	// migration win.
	FieldDirective FieldDirective
	// TagDirective names an SDL directive that adds a struct tag to an
	// input-object field, with the arguments holding the tag's key and value.
	// Empty means none.
	//
	// gqlgen spells it @goTag(key: "valid", value: "required,max=200"), and a
	// service that validates the decoded input with struct tags -- the
	// go-validator `valid` tag, go-playground's `validate` -- reads exactly
	// what the SDL says. Without this option every such tag is dropped and the
	// validator runs over a struct with nothing to validate; nothing fails,
	// because a missing tag is not an error. One real schema carries 5 899.
	//
	// Tags are written after the graphql tag, in the order the directives
	// appear. Only input-object fields take them: an output field is read, not
	// validated. A key or value the struct-tag syntax cannot carry, a key that
	// repeats on one field, and the reserved key "graphql" are errors rather
	// than silently-wrong tags.
	TagDirective TagDirective
	// FieldNames maps an SDL field or argument name to the Go identifier it gets, instead of the
	// name the generator derives. Empty means the derived name for every field.
	//
	// The derived name for a leading underscore keeps an X (`_lastUpdatedAt` is
	// XLastUpdatedAt): an underscore has no upper case, so dropping it would leave the field
	// unexported, and the X keeps `_x` and `x` on one type from colliding. A schema that carries
	// exactly one such name, with no sibling it can collide with, may prefer the plain name
	// gqlgen gave it and its callers already use: `_lastUpdatedAt: LastUpdatedAt`.
	//
	// A bare key applies wherever the SDL name appears, as a field, an input field or an argument.
	// A key written as a schema coordinate -- "User.userId", or "Query.user(byId:)" for an
	// argument -- applies to that one alone, and outranks a bare key for the same name.
	// The value must be an exported Go identifier, and two names that end up identical on one
	// type are refused with their SDL coordinates, as any other collision is.
	FieldNames map[string]string
	// JSONTags writes a json struct tag on every generated model field: the field's GraphQL
	// name, with ",omitempty" when the field is nullable -- what gqlgen wrote.
	//
	// Code that reads the tag by reflection depends on it, and a missing tag is silent: a
	// struct-to-map helper keys its entries by it, a validator names the offending field by
	// it in the error a client sees, a masker finds a field to redact by it, and a setter table
	// matches an aggregate's operations by it. Each falls back to the Go field name or skips the
	// field, so the output is wrong rather than broken.
	//
	// Off by default, which leaves the generated structs untagged.
	JSONTags bool
	// Manifest binds GraphQL types and fields explicitly instead of letting
	// the generator infer them. It loads no Go type information; see the
	// Manifest documentation.
	Manifest *Manifest
	// AutoBind names package patterns to discover bindings from. Only those
	// packages are loaded, and only their export data: no syntax trees and no
	// function bodies. A Manifest entry overrides discovery field by field.
	AutoBind []string
	// Notef receives a line whenever the generator resolves a disagreement it
	// could have resolved another way -- today, a declared enum binding that
	// AutoBind can see would not compile, which is dropped so the enum is
	// modelled instead. A nil func discards them.
	//
	// It is not a log. Every line is a decision the author would want to know
	// about and can act on, which is why there is no level and no filtering:
	// anything that does not meet that bar belongs in an error or nowhere.
	Notef func(format string, args ...any)
	// ZeroForNullInputs emits graphql.ZeroForNull() on every generated Input
	// binding, so a Go field that cannot be null may back a nullable input
	// position and absent, null and the zero value all mean the same thing.
	//
	// It is one setting rather than a list because the schemas that need it
	// need it everywhere: an ORM emits filter inputs by the hundred with
	// `IsNil bool` under `Boolean`, and 11 035 such fields on one real schema
	// are not a list anyone maintains by hand. Read the ZeroForNull godoc
	// before setting it -- for a PATCH-style input, absent and null are
	// different requests and NullableInputOmittable is the option you want.
	//
	// **This is a migration aid, not a design.** The right long-term fix is in
	// the generator that emits the Go type: a nullable SDL field should be
	// backed by a pointer. Setting this teaches an engine invariant to tolerate
	// a generator's convention, and every schema that keeps it set keeps a
	// distinction the GraphQL specification makes and the Go type cannot.
	ZeroForNullInputs bool
	// InlineAccessors binds a field AutoBind resolved to a method on the model
	// that takes no GraphQL arguments -- Order.Customer(ctx), Order.Items(ctx)
	// -- with graphql.Inline(), so it runs in its parent's goroutine instead of
	// on one of its own.
	//
	// For an ORM with field collection these methods answer from edges loaded
	// with the parent, and a goroutine per list element was the largest cost
	// of a page: on a fifty-order page with each order's customer and items,
	// inline was 1.9x faster end to end. The price is that an accessor which
	// does query -- a parent loaded without collection -- queries once per row
	// in sequence rather than concurrently. Methods with arguments, such as a
	// Relay connection that may page per row, are left concurrent.
	InlineAccessors bool
	// Inline names fields, as schema coordinates ("Order.totalCents"), whose
	// resolver runs in its parent's goroutine: one the author knows answers
	// from memory, such as a computed field over an edge the ORM already
	// loaded. A coordinate naming no field fails generation.
	Inline []string
	// Federation makes the schema an Apollo Federation subgraph. The SDL is
	// parsed after fed.Directives, so @key and the rest need no declaration,
	// and the generated NewSchema takes the resolvers the router reaches
	// entities through -- NewSchema(entities []fed.Entity, opts...) -- and
	// serves _service and _entities through fed.SubgraphFS. ValidateSchema
	// stays argument-free: it supplies a placeholder resolver for every @key
	// type, since building never calls one.
	Federation bool
	// NullableInputOmittable uses graphql.Omittable for nullable
	// input-object fields so PATCH-style inputs distinguish absent from
	// null: Omittable[*T] for a value, Omittable[[]T] for a list. Field
	// arguments stay pointers, and so do the elements of a list, which are
	// never absent.
	NullableInputOmittable bool
	// StructObjectFields makes a field of object, interface or union type that
	// takes no arguments a struct field, read from the model like a scalar, on
	// every type whose Go binding is not fully stated (a generated model, or a
	// hand-written one named only in Models). Without it such a field is a
	// Resolver method, which suits an ORM entity that loads relations lazily but
	// leaves a model built by hand nowhere to hold the nested value. A field with
	// arguments is always a resolver, and a type bound through a manifest or
	// AutoBind keeps the bindings it states. Models may then refer to each
	// other across groups; a cycle among them falls back to one shared model
	// package, as cyclic inputs do.
	StructObjectFields bool
	// Wire writes wire.go into Output: one function, Wire(d), that constructs
	// every group's Resolver from one dependencies value and returns their
	// bindings for NewSchema. Empty means none.
	//
	// Something has to construct each group's implementation and hand the list
	// to NewSchema. Written by hand that is a line per group to keep in step
	// with the schema. Left to a dependency container it is reflection at
	// start-up -- about 50 microseconds a provider, measured, so tens of
	// milliseconds at a few hundred groups -- and a group left out is found
	// when the process starts. gqlc knows every group and, from Scaffold,
	// where its implementation is, so it writes the list: a group with no
	// constructor does not compile, and nothing is resolved at run time.
	//
	// Every group with a Resolver needs a Scaffold target, and each
	// implementation a constructor beside it, NewType; scaffolding writes one
	// with a new type, and a note where the type exists without it.
	//
	// What the constructor takes decides what its package imports, and so
	// what recompiles when something changes. A field of the dependencies
	// struct named after a group -- Product for the product group, case and
	// punctuation aside -- is what that group's constructor is given, and its
	// package then imports that field's type alone. Taking the whole struct
	// instead imports everything it names: at 300 groups with a service each,
	// a method added to one service rebuilt 305 packages against 6. A group
	// with no such field, and a type implementing several groups, get the
	// whole value. The struct is read from source; nothing is loaded.
	//
	// A constructor that already exists decides for itself: one written to
	// take the whole struct is passed it, field or no field, which is also
	// how a group that needs more than its own field asks for everything. A
	// field is handed over only when its type can be written exactly in the
	// constructor's package; one with an inline struct or interface type, an
	// unexported type, or a type from a dot-import is passed as part of the
	// whole value instead.
	Wire Wire
	// Check makes Generate write and delete nothing, and report instead: it
	// returns an error wrapping ErrStale that names each file a generate
	// would create, rewrite or delete, or nil when the output is current. It
	// refuses whatever a generate would refuse, so a configuration error is
	// never reported as up to date.
	//
	// Scaffolding is not checked, beyond refusing a scaffold configuration
	// a generate would refuse. It adds to code the author owns, so whether it
	// is all there is the compiler's to say: a missing method at the var _
	// Resolver line, a missing implementation wherever it is used. A file
	// that differs only in line endings is unchanged, here as for a generate.
	Check bool
	// Verify builds every package under Output with the go command once the
	// generate has written it, and fails with what the compiler said.
	//
	// A generate can exit 0 over code that does not compile: a type the
	// config binds by name is not read unless AutoBind is set, so a field is
	// written from the SDL's name and the struct may spell it another way,
	// and a hand-written implementation in the same package may no longer fit
	// the interface. Each is the compiler's to report, at the next build;
	// with Verify the generate that caused it is the one that fails.
	//
	// It is the one place gqlc compiles Go, which is why it is off by
	// default: an unchanged tree costs what the build cache costs, a few
	// seconds at several hundred packages, and a first generate costs a full
	// build. Everything under Output is built on every run, changed or not,
	// so a failure is not reported once and passed the next time. The files
	// are written either way. Check ignores it, having written nothing.
	Verify bool
	// InputListPointers spells a list of input objects []*T instead of []T, as
	// gqlgen did. Callers written against that shape test elements for nil and
	// dereference them; a value slice breaks every one. Scalar and enum lists are
	// unchanged.
	InputListPointers bool
}

// RootField is what Config.RootFieldGroup is told about a root field.
type RootField struct {
	// Root is the root type's name, usually Query, Mutation or Subscription.
	Root string
	// Name is the field's name.
	Name string
	// SDLFile is the file the field is declared in, which for a field added by
	// `extend type` is the extension's file and not the root's.
	SDLFile string
	// ReturnType is the named type the field returns, lists and non-null
	// removed.
	ReturnType string
	// ReturnGroup is the group ReturnType is in, or empty for a built-in
	// scalar, which belongs to no group.
	ReturnGroup string
}

// ModelDirective identifies an SDL directive carrying a Go type binding.
type ModelDirective struct {
	// Name is the directive, without the @.
	Name string
	// Arg is the argument holding the Go type expression.
	Arg string
}

// IsZero reports that no directive was named.
func (d ModelDirective) IsZero() bool { return d.Name == "" || d.Arg == "" }

// FieldDirective names an SDL directive that forces a field to the Resolver
// interface.
type FieldDirective struct {
	// Name is the directive, without the @.
	Name string
	// ForceResolverArg is the boolean argument that forces the resolver.
	ForceResolverArg string
}

// IsZero reports that no directive was named.
func (d FieldDirective) IsZero() bool { return d.Name == "" || d.ForceResolverArg == "" }

// Wire configures the generated Wire function.
type Wire struct {
	// Deps is the Go type Wire takes, written as import/path.Type: the
	// application's shared dependencies -- a database client, configuration,
	// services. A constructor that exists is given what its parameters are
	// named after: none is called with nothing, and each parameter is given
	// the field it is named after, or the whole value for one of this type.
	// A single parameter named after no field, and a constructor gqlc
	// scaffolds, are given the field named after the group, or the whole
	// value where there is none. Its package must not import the generated
	// one, which imports it.
	Deps string
	// Pass says outright what a group's constructor is given, by scaffold
	// key, as the expression wire.go writes: "d" for the whole value, "&d"
	// for a pointer to it, "d.Field" for a field, "&d.Field" for a pointer to
	// one. A group not listed is decided as Config.Wire describes.
	//
	// That decision is read from source without type information -- from the
	// struct, and from a constructor that exists -- and a reading can be
	// wrong: a constructor taking an interface the whole value satisfies, a
	// field named for something other than its group. A wrong one lands in
	// wire.go, which is not to be edited. An entry here is what wire.go
	// writes, whatever the source looks like: a constructor that takes
	// something else is a compile error naming it, and one gqlc scaffolds is
	// written to take exactly this.
	//
	// Wire takes the dependencies by value, so a pointer is to Wire's own
	// copy. A type implementing several groups has one constructor, and one
	// entry under any of those groups speaks for all of them. An entry that
	// cannot be meant is an error from Generate: a key that is no group, an
	// unexported field of another package, two entries that differ for one
	// type, and a field whose type gqlc would have to write into a
	// constructor and cannot. A field that does not exist is one too, but
	// only where every field can be read: a struct declared once, in this
	// module, that embeds nothing. Otherwise the compiler reports it.
	Pass map[string]string
}

// IsZero reports whether Wire is not configured.
func (w Wire) IsZero() bool { return w.Deps == "" }

// TagDirective names an SDL directive that adds a struct tag to an
// input-object field.
type TagDirective struct {
	// Name is the directive, without the @.
	Name string
	// KeyArg is the string argument holding the tag key.
	KeyArg string
	// ValueArg is the string argument holding the tag value.
	ValueArg string
}

// IsZero reports that no directive was named.
func (d TagDirective) IsZero() bool { return d.Name == "" || d.KeyArg == "" || d.ValueArg == "" }
