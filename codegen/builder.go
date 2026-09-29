package codegen

import (
	"fmt"
	"github.com/syssam/graphql-go/fed"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

type builder struct {
	cfg      Config
	dir      string
	schema   *ast.Schema
	sources  []*ast.Source
	pkgName  string
	modelImp string

	// modelSplit emits one model package per group instead of a single
	// shared one. Set by emit once the groups are known.
	modelSplit bool

	// manifest is nil unless Config.Manifest was given. Its type bindings are
	// already folded into cfg.Models, so only field kinds and group overrides
	// are read from here.
	manifest *manifest

	// namesByKind and groupByType memoize two answers that do not change once
	// the schema is loaded. Both are asked once per group, and emit runs per
	// group, so at 800 groups over 4 800 types they were the difference between
	// linear and quadratic: typeNames alone scanned and sorted every type on
	// each of its ten-odd call sites.
	namesByKind map[ast.DefinitionKind][]string
	groupByType map[string]string
	// groupByRootField memoizes rootFieldGroup, keyed "Root.field".
	groupByRootField map[string]string
	// fieldsByGroup indexes every object field by group, "" holding all of
	// them; groups memoizes uniqueGroups. Both are built in emit, after the
	// model map is final, and fieldScans and groupScans count the builds.
	fieldsByGroup map[string][]groupField
	groups        []string
	fieldScans    int
	groupScans    int
	// marshalers are the SDL enums and scalars whose declared Go type encodes
	// itself; they bind through EnumMarshaler and ScalarMarshaler. Only
	// AutoBind loads the types that can say so.
	marshalers map[string]bool

	// nameScans counts how often typeNames actually scanned, which is what
	// TestTypeNamesIsComputedOncePerKind reads: once per kind and not once
	// per group is the difference between linear and quadratic here.
	nameScans int

	// exprImports memoizes modelExprImports, which modelQualifier asks for
	// once per type reference while itself scanning every Models entry. With
	// AutoBind that map holds an entry per discovered type, so recomputing it
	// was references times types. setModels clears it; nothing else may write
	// cfg.Models.
	exprImports map[string]string
	// exprQualifiers is its inverse, import path to qualifier, filled with it.
	exprQualifiers map[string]string
	exprScans      int
	// scaffoldParses counts the files scaffold parses, which must be each
	// file once per run however many groups share a package.
	scaffoldParses int

	// loads counts calls to packages.Load. "It reads no Go type information
	// without AutoBind" is a promise the documentation makes and only the
	// `len(cfg.AutoBind) > 0` guard keeps; loading a module is the cost this
	// generator exists to avoid, so a regression here is the whole design
	// going quietly. TestNoPackagesAreLoadedWithoutAutoBind reads it.
	loads int

	// markers memoizes hasMarker. setModels clears it, because whether a
	// member is mapped is what decides it.
	markers map[string]bool

	// extraEnums holds the second Go type some SDL enums have, rendered as
	// the references generated code will use, keyed by SDL enum name. See
	// Manifest.ExtraEnums. extraImports carries the packages they need, which
	// neither Config.Models nor the generated model packages name.
	extraEnums   map[string][]extraEnum
	extraImports map[string]string
}

// extraEnum is one second binding for an SDL enum, already qualified: ref is
// the Go type as generated code writes it, and consts maps each SDL value to
// the constant reference that carries it.
type extraEnum struct {
	ref    string
	consts map[string]string
}

// notef reports a decision through Config.Notef, if there is one.
func (b *builder) notef(format string, args ...any) {
	if b.cfg.Notef != nil {
		b.cfg.Notef(format, args...)
	}
}

// setModels replaces the model map and drops what was memoized from it.
func (b *builder) setModels(models map[string]string) {
	b.cfg.Models = models
	b.exprImports = nil
	b.exprQualifiers = nil
	b.markers = nil
}

// hasMarker reports whether the interface or union named gets a generated Go
// marker interface -- `type Node interface{ IsNode() }`, with the method on
// every member -- instead of being any.
//
// The method has to be declared on each member, and Go allows that only in
// the member's own package, so every member must be a model this generator
// writes: one mapped through Models, a directive or AutoBind is someone else's
// type. An abstract type that is mapped itself already has a Go type. A member
// with a field whose Go name is the method's would not compile, so that falls
// back to any too.
func (b *builder) hasMarker(name string) bool {
	if v, ok := b.markers[name]; ok {
		return v
	}
	if b.markers == nil {
		b.markers = map[string]bool{}
	}
	ok := b.computeMarker(name)
	b.markers[name] = ok
	return ok
}

func (b *builder) computeMarker(name string) bool {
	def := b.schema.Types[name]
	if def == nil || (def.Kind != ast.Interface && def.Kind != ast.Union) || b.mapped(name) {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(name); !unicode.IsUpper(r) {
		return false
	}
	method := b.markerMethod(name)
	for _, member := range b.schema.PossibleTypes[name] {
		// An interface implementing this one carries its method in its own
		// marker (markerMethods); only objects have methods declared on them.
		if member.Kind == ast.Interface {
			continue
		}
		if member.Kind != ast.Object || b.mapped(member.Name) || b.isRoot(member.Name) {
			return false
		}
		for _, fd := range member.Fields {
			if b.fieldKind(member.Name, fd) == fieldPure && goIdent(fd.Name) == method {
				return false
			}
		}
	}
	return true
}

func (b *builder) markerMethod(name string) string { return "Is" + name }

// markerMethods is the method set of name's marker: its own method, then those
// of every interface it implements that has a marker too, so a value of it can
// be returned where one of those is asked for.
func (b *builder) markerMethods(name string) []string {
	var out []string
	if def := b.schema.Types[name]; def != nil {
		for _, parent := range def.Interfaces {
			if b.hasMarker(parent) {
				out = append(out, b.markerMethod(parent))
			}
		}
	}
	return append(out, b.markerMethod(name))
}

func newBuilder(dir string, cfg Config) (*builder, error) {
	var srcs []*ast.Source
	for _, glob := range cfg.SchemaGlobs {
		pattern := glob
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(dir, glob)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("codegen: glob %q: %w", glob, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("codegen: glob %q matched no files", glob)
		}
		slices.Sort(matches)
		for _, path := range matches {
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			srcs = append(srcs, &ast.Source{Name: filepath.ToSlash(path), Input: string(raw)})
		}
	}
	load := srcs
	if cfg.Federation {
		// BuiltIn: the directives are the protocol's, not the author's, and
		// nothing is generated for them.
		load = append([]*ast.Source{{Name: "federation.graphql", Input: fed.Directives, BuiltIn: true}}, srcs...)
	}
	sch, err := gqlparser.LoadSchema(load...)
	if err != nil {
		return nil, fmt.Errorf("codegen: load schema: %w", err)
	}
	pkgName := cfg.Package
	if i := strings.LastIndex(pkgName, "/"); i >= 0 {
		pkgName = pkgName[i+1:]
	}
	b := &builder{
		cfg:      cfg,
		dir:      dir,
		schema:   sch,
		sources:  srcs,
		pkgName:  pkgName,
		modelImp: cfg.Package + "/model",
	}

	// Bindings the SDL already carries, folded in before the manifest so an
	// explicit Models entry still wins over what a directive says.
	if !cfg.ModelDirective.IsZero() {
		cfg.Models = foldModelDirective(sch, cfg.ModelDirective, cfg.Models)
		// foldModelDirective returns a new map, and discovery below reads
		// the builder's copy rather than this one. Without this line every
		// field whose type the SDL binds is measured against the model the
		// generator would have written instead, and none of them match.
		b.setModels(cfg.Models)
	}

	// Discovery produces a manifest and then stops, so everything downstream
	// is the manifest path. It runs against the builder as it stands, because
	// deciding whether a Go field can answer a GraphQL field means knowing
	// what Go type that field needs, which only goType can say. Discovery only
	// ever adds object-type mappings, so the leaf types it asks about are
	// already settled.
	explicit := cfg.Manifest
	if len(cfg.AutoBind) > 0 {
		discovered, aerr := autoBind(dir, cfg.AutoBind, sch, b)
		if aerr != nil {
			return nil, aerr
		}
		// The builder's map, not the local one: discovery drops a declared
		// binding it can see would not compile, and the local copy still
		// carries it.
		explicit = mergeManifests(yieldToDeclared(discovered, b.cfg.Models), explicit)
		cfg.Models = b.cfg.Models
	}

	// Folding the manifest's type bindings into cfg.Models here means model
	// references, imports and the mapped check keep working unchanged; the
	// manifest is only consulted afterwards for field kinds and groups.
	man, models, err := newManifest(explicit, sch, cfg.Models)
	if err != nil {
		return nil, fmt.Errorf("codegen: %w", err)
	}
	b.setModels(models)
	b.manifest = man
	b.setExtraEnums(explicit)
	return b, nil
}

// setExtraEnums renders Manifest.ExtraEnums into the references generated code
// will carry, and reserves an import qualifier for each package they come
// from. The qualifier is settled here, once, because the emitter writes the
// reference and the import block from the same two maps -- deciding it twice
// is how a reference and its import disagree.
func (b *builder) setExtraEnums(man *Manifest) {
	if man == nil || len(man.ExtraEnums) == 0 {
		return
	}
	b.extraEnums = map[string][]extraEnum{}
	b.extraImports = map[string]string{}
	for _, tb := range man.ExtraEnums {
		if tb.Go.zero() || len(tb.Values) == 0 {
			continue
		}
		q := b.extraQualifier(tb.Go.PkgPath)
		e := extraEnum{ref: q + "." + tb.Go.Name, consts: map[string]string{}}
		for value, ident := range tb.Values {
			e.consts[value] = q + "." + ident
		}
		b.extraEnums[tb.Name] = append(b.extraEnums[tb.Name], e)
	}
}

// extraQualifier picks the name an extra enum's package is imported under, and
// registers the import unless one already covers it.
//
// A package Config.Models already names is imported by that path, under that
// qualifier, so registering it again emits the import twice -- which is a
// redeclaration, not a warning, in a file marked DO NOT EDIT. A name spoken
// for by a *different* package gets a suffix instead of losing, because both
// have to be referable at once.
func (b *builder) extraQualifier(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ReplaceAll(base, "-", "")
	if base == "" {
		base = "enum"
	}
	for q, have := range b.extraImports {
		if have == path {
			return q
		}
	}
	for q, have := range b.modelExprImports() {
		if have == path {
			return q
		}
	}
	taken := func(q string) bool {
		if p, ok := b.modelExprImports()[q]; ok && p != path {
			return true
		}
		_, ok := b.extraImports[q]
		return ok
	}
	q := base
	for i := 2; taken(q); i++ {
		q = base + "enum"
		if i > 2 {
			q = base + "enum" + strconv.Itoa(i)
		}
	}
	b.extraImports[q] = path
	return q
}

// typeNames returns the schema's type names of one kind, sorted. The slice is
// memoized and shared, so callers range over it and must not write to it.
func (b *builder) typeNames(kind ast.DefinitionKind) []string {
	if names, ok := b.namesByKind[kind]; ok {
		return names
	}
	var names []string
	for name, def := range b.schema.Types {
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if def.Kind == kind {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	b.nameScans++
	if b.namesByKind == nil {
		b.namesByKind = make(map[ast.DefinitionKind][]string, 6)
	}
	b.namesByKind[kind] = names
	return names
}

func (b *builder) isRoot(name string) bool {
	s := b.schema
	return (s.Query != nil && s.Query.Name == name) ||
		(s.Mutation != nil && s.Mutation.Name == name) ||
		(s.Subscription != nil && s.Subscription.Name == name)
}

// isSubscriptionRoot reports whether name is the subscription root, whose
// fields bind to a source stream rather than to a resolver.
func (b *builder) isSubscriptionRoot(name string) bool {
	s := b.schema
	return s.Subscription != nil && s.Subscription.Name == name
}

func (b *builder) named(t *ast.Type) *ast.Definition {
	if t == nil {
		return nil
	}
	return b.schema.Types[t.Name()]
}

func (b *builder) fieldKind(typeName string, fd *ast.FieldDefinition) fieldKind {
	// A root object's parent value is graphql.Root, which holds no data, and
	// no model is generated for it. Every root field therefore has to come
	// from the resolver, however scalar and argument-less it looks.
	if b.isRoot(typeName) {
		return fieldResolve
	}
	// The author saying a field is computed outranks the generator seeing
	// that it could be read.
	if b.forcedResolver(fd) {
		return fieldResolve
	}
	// An explicit binding wins, and a type in the manifest gets no inference
	// at all: its unlisted fields are resolvers, so an incomplete manifest
	// surfaces as a missing resolver method rather than as generated access
	// to a struct field that may not exist.
	if b.manifest.bound(typeName) {
		if fb, ok := b.manifest.binding(typeName, fd.Name); ok && fb.pure() {
			return fieldPure
		}
		return fieldResolve
	}
	if len(fd.Arguments) > 0 {
		return fieldResolve
	}
	named := b.named(fd.Type)
	if named == nil {
		return fieldResolve
	}
	if named.Kind == ast.Scalar || named.Kind == ast.Enum {
		return fieldPure
	}
	return fieldResolve
}

type fieldKind int

const (
	fieldPure fieldKind = iota
	fieldResolve
)

func goIdent(name string) string {
	if name == "id" {
		return "ID"
	}
	// A leading underscore has no upper case, so upper-casing the first rune
	// left `_lastUpdatedAt` unexported -- and Input[T] skips unexported
	// fields, so the input could never bind and NewSchema refused the whole
	// schema. The generated Go compiled perfectly; only building the schema
	// found it, 342 times on one real schema. A leading underscore is legal
	// in SDL and conventional for a meta field, so it has to produce an
	// exported name rather than be rejected.
	//
	// The X stays rather than being dropped, so that `_x` and `x` on one type
	// do not both become X and collide -- the `graphql` tag pins the SDL name,
	// so a collision would be a compile error in generated code.
	trimmed := strings.TrimLeft(name, "_")
	if trimmed == "" {
		return "X"
	}
	r, w := utf8.DecodeRuneInString(trimmed)
	s := string(unicode.ToUpper(r)) + trimmed[w:]
	if strings.HasSuffix(s, "Id") {
		s = s[:len(s)-2] + "ID"
	}
	// Prefix when the name was underscored, or when upper-casing cannot
	// export it at all -- a digit or a symbol first.
	if trimmed != name || !unicode.IsUpper(unicode.ToUpper(r)) {
		s = "X" + s
	}
	return s
}

// modelPkgOf names the package a generated model type lives in: the single
// "model" package, or the type's own group when models are split.
// models returns the effective model map: Config.Models merged with the
// manifest's type bindings.
func (b *builder) modelPkgOf(graphqlName string) string {
	if !b.modelSplit {
		return "model"
	}
	return b.groupOf(graphqlName)
}

// modelImportOf is the import path for that package.
func (b *builder) modelImportOf(pkg string) string {
	if pkg == "model" {
		return b.modelImp
	}
	return b.modelImp + "/" + pkg
}

// modelName renders a reference to a model type. selfPkg is the model package
// the current file lives in, whose types need no qualifier; it is empty for
// every file outside the model tree.
func (b *builder) modelName(graphqlName, selfPkg string) string {
	if expr, ok := b.cfg.Models[graphqlName]; ok {
		return b.exprRef(expr)
	}
	switch graphqlName {
	case "String":
		return "string"
	case "Int":
		return "int"
	case "Float":
		return "float64"
	case "Boolean":
		return "bool"
	case "ID":
		return "graphql.ID"
	}
	def := b.schema.Types[graphqlName]
	if def != nil && (def.Kind == ast.Interface || def.Kind == ast.Union) && !b.hasMarker(graphqlName) {
		return "any"
	}
	pkg := b.modelPkgOf(graphqlName)
	if pkg == selfPkg {
		return graphqlName
	}
	return b.modelQualifier(pkg) + "." + graphqlName
}

// modelQualifier is the identifier a generated model package is referred to by.
//
// It is the package name, unless a type mapped through Models comes from a Go
// package with the same base name -- one real schema had a billing group whose
// models sat in graph/model/billing while its mapped types came from
// app/billing, and the generated file imported both under the name billing.
// That is a redeclaration, and every reference after it resolves to the wrong
// package or to nothing: 410 redeclarations and most of 1 983 undefined
// symbols across 381 packages, from this one cause.
//
// The generated side is the one that yields, because it is the side whose
// references this generator writes.
func (b *builder) modelQualifier(pkg string) string {
	if _, taken := b.modelExprImports()[pkg]; taken {
		return pkg + "model"
	}
	// And to an extra enum's package, for the same reason: it is the author's
	// package, reached through their own struct, where this one is a name the
	// generator chose. A group called "method" beside a second enum type in
	// .../method put two imports under one qualifier, which is a redeclaration
	// rather than a warning (TestSecondEnumPackageDoesNotCollideWithAGeneratedModelPackage).
	if _, taken := b.extraImports[pkg]; taken {
		return pkg + "model"
	}
	return pkg
}

func (b *builder) goType(t *ast.Type, selfPkg string, omitNull bool) string {
	if t.Elem != nil {
		inner := b.goType(t.Elem, selfPkg, omitNull)
		if t.NonNull {
			return "[]" + inner
		}
		return "[]" + inner
	}
	base := b.modelName(t.NamedType, selfPkg)
	def := b.schema.Types[t.NamedType]
	if def != nil && (def.Kind == ast.Object || def.Kind == ast.Interface || def.Kind == ast.Union) {
		// An object's Go side is a struct, so it takes a pointer. An abstract
		// type's is itself an interface, and *Noder satisfies nothing -- the
		// executor resolves the concrete type from the dynamic one. An
		// unmapped abstract type is `any` and never reached this, which is
		// why the pointer went unnoticed until a schema mapped one.
		if def.Kind == ast.Object && base != "any" && !strings.HasPrefix(base, "*") {
			base = "*" + base
		}
		return base
	}
	if !t.NonNull {
		if omitNull {
			return "graphql.Omittable[*" + strings.TrimPrefix(base, "*") + "]"
		}
		if !strings.HasPrefix(base, "*") && base != "any" {
			return "*" + base
		}
	}
	return base
}

func (b *builder) argsName(typeName, field string) string {
	if b.isRoot(typeName) {
		return goIdent(field) + "Args"
	}
	return typeName + goIdent(field) + "Args"
}

func (b *builder) resolverMethod(typeName, field string) string {
	if b.isRoot(typeName) {
		return goIdent(field)
	}
	return typeName + goIdent(field)
}

// modelRef qualifies a generated model identifier for use outside the model
// tree: typeName selects the package, ident is the type or a constant
// declared beside it.
//
// A type mapped through Config.Models lives in the caller's package, not the
// generated model package, so the mapping decides the qualifier here too.
// Without this the object binding and its resolver receiver pointed at a
// generated model while the field types pointed at the mapped one.
func (b *builder) modelRef(typeName, ident string) string {
	if expr, ok := b.cfg.Models[typeName]; ok {
		ref := b.exprRef(expr)
		if ident == typeName {
			return ref
		}
		if i := strings.LastIndex(ref, "."); i >= 0 {
			return ref[:i+1] + ident
		}
		return ident
	}
	return b.modelQualifier(b.modelPkgOf(typeName)) + "." + ident
}

// splitModelExpr separates a Config.Models entry into the package it must be
// imported from and the reference to write in generated code:
//
//	"time.Time"                  -> "time",        "time.Time"
//	"example.com/x/shop.Product" -> "example.com/x/shop", "shop.Product"
//	"string"                     -> "",            "string"
//
// Without this the whole import path was written inline, which is not valid
// Go, so any model outside a package the generator already imports failed.
func splitModelExpr(expr string) (importPath, ref string) {
	bare := strings.TrimLeft(expr, "*[]")
	prefix := expr[:len(expr)-len(bare)]
	slash := strings.LastIndex(bare, "/")
	dot := strings.Index(bare[slash+1:], ".")
	if dot < 0 {
		return "", expr
	}
	pkgEnd := slash + 1 + dot
	return bare[:pkgEnd], prefix + bare[slash+1:]
}

// modelExprImports maps the package qualifier used in generated code to the
// path it comes from, for every Config.Models entry that needs an import.
//
// The qualifier is decided here, once per import path, and the import block
// always writes it as an alias. Reading it off the reference instead assumed a
// package is named after its directory and that no two mapped paths share a
// last element; an ORM breaks both at once, with its entity enums in
// .../todo and its mutation inputs in .../client/todo under package
// todoclient (TestMappedPackagesGetTheirOwnQualifier).
func (b *builder) modelExprImports() map[string]string {
	if b.exprImports != nil {
		return b.exprImports
	}
	b.exprScans++
	paths := map[string]bool{}
	for _, expr := range b.cfg.Models {
		if path, _ := splitModelExpr(expr); path != "" {
			paths[path] = true
		}
	}
	out := map[string]string{}
	b.exprQualifiers = map[string]string{}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		q := pathQualifier(path, out)
		out[q] = path
		b.exprQualifiers[path] = q
	}
	b.exprImports = out
	return out
}

// reservedQualifiers are the names every generated file may already import
// under, so no mapped package may take one.
var reservedQualifiers = map[string]bool{"graphql": true, "context": true}

// pathQualifier picks an identifier for an import path that taken and
// reservedQualifiers do not already hold: the last path element, then the
// last two joined, then a numeric suffix. A major-version element (v2) is
// skipped, because it names no package.
func pathQualifier(path string, taken map[string]string) string {
	elems := strings.Split(path, "/")
	if n := len(elems); n > 1 && isMajorVersion(elems[n-1]) {
		elems = elems[:n-1]
	}
	base := identFrom(elems[len(elems)-1])
	free := func(q string) bool {
		_, used := taken[q]
		return q != "" && !used && !reservedQualifiers[q]
	}
	if free(base) {
		return base
	}
	if len(elems) > 1 {
		if q := identFrom(elems[len(elems)-2]) + base; free(q) {
			return q
		}
	}
	for i := 2; ; i++ {
		if q := base + strconv.Itoa(i); free(q) {
			return q
		}
	}
}

func isMajorVersion(elem string) bool {
	if len(elem) < 2 || elem[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(elem[1:])
	return err == nil
}

// identFrom keeps the letters and digits of a path element, lower-cased, so
// go-yaml and yaml.v3 still give a usable identifier.
func identFrom(elem string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(elem) {
		if r == '_' || unicode.IsLetter(r) || (unicode.IsDigit(r) && sb.Len() > 0) {
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 0 {
		return "pkg"
	}
	return sb.String()
}

// exprRef renders a Config.Models entry as generated code refers to it, under
// the qualifier modelExprImports chose for its package.
func (b *builder) exprRef(expr string) string {
	path, ref := splitModelExpr(expr)
	if path == "" {
		return ref
	}
	b.modelExprImports()
	bare := strings.TrimLeft(ref, "*[]")
	_, typ, _ := strings.Cut(bare, ".")
	return ref[:len(ref)-len(bare)] + b.exprQualifiers[path] + "." + typ
}

// mapped reports whether a type is supplied by the caller through
// Config.Models, in which case no model is generated for it.
func (b *builder) mapped(graphqlName string) bool {
	_, ok := b.cfg.Models[graphqlName]
	return ok
}

// foldModelDirective reads a type-binding directive off the schema into the
// Models map. An entry already in Models is left alone: a config file is a
// deliberate override of what the SDL happens to say.
//
// A directive with no argument, or one whose argument is not a string, is
// skipped rather than reported. gqlgen's @goModel also accepts models: [..]
// and forceGenerate:, and a schema that uses those forms on a type simply does
// not get a binding from here -- it can still be named in Models.
func foldModelDirective(sch *ast.Schema, d ModelDirective, models map[string]string) map[string]string {
	out := make(map[string]string, len(models)+len(sch.Types))
	maps.Copy(out, models)
	names := make([]string, 0, len(sch.Types))
	for name := range sch.Types {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		def := sch.Types[name]
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		dir := def.Directives.ForName(d.Name)
		if dir == nil {
			continue
		}
		arg := dir.Arguments.ForName(d.Arg)
		if arg == nil || arg.Value == nil || arg.Value.Kind != ast.StringValue {
			continue
		}
		if expr := strings.TrimSpace(arg.Value.Raw); expr != "" {
			out[name] = expr
		}
	}
	return out
}

// yieldToDeclared drops the Go type a discovery found for a type that is
// already declared in Models, keeping whatever else the discovery learned
// about it.
//
// AutoBind discovers; Models and a binding directive declare. A disagreement
// between two declarations is a mistake and stays an error, but a discovery
// that disagrees with a declaration is not one -- it is the declaration doing
// its job. The two only meet once a schema carries its bindings in the SDL:
// before ModelDirective, Models was empty on this path and the discovery had
// nothing to disagree with.
func yieldToDeclared(discovered *Manifest, models map[string]string) *Manifest {
	if discovered == nil || len(models) == 0 {
		return discovered
	}
	out := &Manifest{Types: make([]TypeBinding, len(discovered.Types)), ExtraEnums: discovered.ExtraEnums}
	copy(out.Types, discovered.Types)
	for i := range out.Types {
		if _, declared := models[out.Types[i].Name]; declared {
			out.Types[i].Go = GoType{}
		}
	}
	return out
}

// modelAnswers reports whether the model itself answers this field, and
// with what binding. Three places ask -- the field kind, the emitted call
// and the Resolver interface -- and they must agree: honouring a forced
// resolver in the call alone emits a Resolve for a method the interface
// never declares, which does not compile.
func (b *builder) modelAnswers(typeName string, fd *ast.FieldDefinition) (FieldBinding, bool) {
	fb, ok := b.manifest.binding(typeName, fd.Name)
	if !ok || fb.Kind == FieldResolver || b.forcedResolver(fd) {
		return FieldBinding{}, false
	}
	return fb, true
}

// forcedResolver reports whether the field carries Config.FieldDirective
// with its argument true.
func (b *builder) forcedResolver(fd *ast.FieldDefinition) bool {
	if b.cfg.FieldDirective.IsZero() || fd == nil {
		return false
	}
	d := fd.Directives.ForName(b.cfg.FieldDirective.Name)
	if d == nil {
		return false
	}
	arg := d.Arguments.ForName(b.cfg.FieldDirective.ForceResolverArg)
	return arg != nil && arg.Value != nil && arg.Value.Kind == ast.BooleanValue && arg.Value.Raw == "true"
}
