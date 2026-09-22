package codegen

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

	// nameScans counts how often typeNames actually scanned, which is what
	// TestTypeNamesIsComputedOncePerKind reads: once per kind and not once
	// per group is the difference between linear and quadratic here.
	nameScans int
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
	sch, err := gqlparser.LoadSchema(srcs...)
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
	}

	// Discovery produces a manifest and then stops, so everything downstream
	// is the manifest path. It runs against the builder as it stands, because
	// deciding whether a Go field can answer a GraphQL field means knowing
	// what Go type that field needs, which only goType can say. Discovery only
	// ever adds object-type mappings, so the leaf types it asks about are
	// already settled.
	explicit := cfg.Manifest
	if len(cfg.AutoBind) > 0 {
		discovered, aerr := autoBind(dir, cfg.AutoBind, sch, b.goType)
		if aerr != nil {
			return nil, aerr
		}
		explicit = mergeManifests(yieldToDeclared(discovered, cfg.Models), explicit)
	}

	// Folding the manifest's type bindings into cfg.Models here means model
	// references, imports and the mapped check keep working unchanged; the
	// manifest is only consulted afterwards for field kinds and groups.
	man, models, err := newManifest(explicit, sch, cfg.Models)
	if err != nil {
		return nil, fmt.Errorf("codegen: %w", err)
	}
	b.cfg.Models = models
	b.manifest = man
	return b, nil
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
	r, w := utf8.DecodeRuneInString(name)
	s := string(unicode.ToUpper(r)) + name[w:]
	if strings.HasSuffix(s, "Id") {
		s = s[:len(s)-2] + "ID"
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
		_, ref := splitModelExpr(expr)
		return ref
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
	if def != nil && (def.Kind == ast.Interface || def.Kind == ast.Union) {
		return "any"
	}
	pkg := b.modelPkgOf(graphqlName)
	if pkg == selfPkg {
		return graphqlName
	}
	return pkg + "." + graphqlName
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
		if base != "any" && !strings.HasPrefix(base, "*") {
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
		_, ref := splitModelExpr(expr)
		if ident == typeName {
			return ref
		}
		if i := strings.LastIndex(ref, "."); i >= 0 {
			return ref[:i+1] + ident
		}
		return ident
	}
	return b.modelPkgOf(typeName) + "." + ident
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
func (b *builder) modelExprImports() map[string]string {
	out := map[string]string{}
	for _, expr := range b.cfg.Models {
		path, ref := splitModelExpr(expr)
		if path == "" {
			continue
		}
		if i := strings.Index(ref, "."); i >= 0 {
			out[strings.TrimLeft(ref[:i], "*[]")] = path
		}
	}
	return out
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
	out := &Manifest{Types: make([]TypeBinding, len(discovered.Types))}
	copy(out.Types, discovered.Types)
	for i := range out.Types {
		if _, declared := models[out.Types[i].Name]; declared {
			out.Types[i].Go = GoType{}
		}
	}
	return out
}
