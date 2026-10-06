package codegen

import (
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

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
		// An element is never Omittable, whatever the field is: Omittable says
		// whether a field was sent, an element of a list always is, and the
		// engine binds E or *E there and nothing else. Passing omitNull down
		// compiled and left NewSchema refusing every input with a list of
		// nullable elements.
		inner := b.goType(t.Elem, selfPkg, false)
		if b.cfg.InputListPointers && t.Elem.Elem == nil {
			if def := b.schema.Types[t.Elem.NamedType]; def != nil && def.Kind == ast.InputObject {
				// An element is *T whether or not it may be null.
				if !strings.HasPrefix(inner, "*") {
					inner = "*" + inner
				}
			}
		}
		// The list itself is a field, and a nullable one is wrapped as any
		// other is: nil stands for both a list left out and one sent as null,
		// which a PATCH-style input has to tell apart.
		if omitNull && !t.NonNull {
			return "graphql.Omittable[[]" + inner + "]"
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
			// A Go type that is nil by itself needs no pointer to be null:
			// a scalar mapped to map[string]any or to any was wrapped as
			// Omittable[*map[string]any], a pointer to a map, which no caller
			// wants to take the address of a literal to fill.
			if base == "any" || strings.HasPrefix(base, "map[") || strings.HasPrefix(base, "[]") {
				return "graphql.Omittable[" + base + "]"
			}
			return "graphql.Omittable[*" + strings.TrimPrefix(base, "*") + "]"
		}
		if !strings.HasPrefix(base, "*") && base != "any" {
			return "*" + base
		}
	}
	return base
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

// mapped reports whether a type is supplied by the caller through
// Config.Models, in which case no model is generated for it.
func (b *builder) mapped(graphqlName string) bool {
	_, ok := b.cfg.Models[graphqlName]
	return ok
}
