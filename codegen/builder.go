package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
		sort.Strings(matches)
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
	return &builder{
		cfg:      cfg,
		dir:      dir,
		schema:   sch,
		sources:  srcs,
		pkgName:  pkgName,
		modelImp: cfg.Package + "/model",
	}, nil
}

func (b *builder) typeNames(kind ast.DefinitionKind) []string {
	var names []string
	for name, def := range b.schema.Types {
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if def.Kind == kind {
			names = append(names, name)
		}
	}
	sort.Strings(names)
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
