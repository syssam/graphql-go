package codegen

import (
	"go/types"
	"strings"
)

// reconcile decides whether a Go type can answer a field needing the Go type
// spelled expected, and whether a conversion is required to do it.
//
// A named type and its underlying basic type are distinct to Go but the same
// value to a schema: an ORM storing an id as a plain string still answers an
// ID! field, and refusing that would send the single most common field in any
// schema through a resolver. A conversion is emitted only when both sides bottom
// out in the same basic kind, so it can neither lose information nor reinterpret
// one thing as another. Anything else is left to the resolver.
// reconcileValue is reconcile for a nullable SDL position: a Go value that is
// always present satisfies it, so "*float64 expected, float64 found" is a
// match and the field is emitted returning the value type. Only the pointer
// is dropped -- the pointee still has to reconcile.
func reconcileValue(actual types.Type, expected string, qual types.Qualifier) (convert, usable bool) {
	if !strings.HasPrefix(expected, "*") {
		return false, false
	}
	return reconcile(actual, expected[1:], qual)
}

func reconcile(actual types.Type, expected string, qual types.Qualifier) (convert, usable bool) {
	if actual == nil || expected == "" {
		return false, false
	}
	if types.TypeString(actual, qual) == expected {
		return false, true
	}
	want, ok := basicClassOf(expected)
	if !ok {
		return false, false
	}
	basic, ok := types.Unalias(actual).Underlying().(*types.Basic)
	if !ok {
		return false, false
	}
	return true, basic.Info()&want != 0
}

// basicClassOf maps a Go type expression the generator emits to the class of
// basic type it bottoms out in. Only leaf types appear here; a list or a
// pointer has no class and is required to match exactly.
func basicClassOf(expr string) (types.BasicInfo, bool) {
	switch expr {
	case "string", "graphql.ID":
		return types.IsString, true
	case "int", "int32", "int64":
		return types.IsInteger, true
	case "float64", "float32":
		return types.IsFloat, true
	case "bool":
		return types.IsBoolean, true
	}
	return 0, false
}

// goQualifier renders a loaded type's package the way generated code spells
// it, so a Go type and the expression the generator wants can be compared as
// strings. A mapped package is spelled with the qualifier modelExprImports gave
// it, which need not be its name; any other by its name, unless that name is a
// mapped package's qualifier, when two different types would read alike.
//
// One of the generator's own model packages is spelled as modelName spells
// it, which is its name only until a mapped package shares that name. A
// struct written by hand holds the generated types -- an enum, another
// object's model -- and read by package name a field of one did not match the
// type the schema wanted, so it fell to a Resolver the struct already answered.
func (b *builder) goQualifier(p *types.Package) string {
	imports := b.modelExprImports()
	if q, ok := b.exprQualifiers[p.Path()]; ok {
		return q
	}
	if p.Path() == b.modelImp || strings.HasPrefix(p.Path(), b.modelImp+"/") {
		// Whether models are split by group is decided after discovery, and
		// until then modelName puts every one of them in "model". Spelled
		// the same way here, the two sides compare by the type's name, which
		// a schema has once.
		pkg := "model"
		if b.modelSplit {
			pkg = p.Path()[strings.LastIndex(p.Path(), "/")+1:]
		}
		return b.modelQualifier(pkg)
	}
	if path, taken := imports[p.Name()]; taken && path != p.Path() {
		return p.Path()
	}
	return p.Name()
}
