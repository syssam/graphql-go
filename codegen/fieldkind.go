package codegen

import (
	"github.com/vektah/gqlparser/v2/ast"
)

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
	if fb, ok := b.manifest.binding(typeName, fd.Name); ok {
		if fb.pure() {
			return fieldPure
		}
		return fieldResolve
	}
	if b.manifest.bound(typeName) {
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
	if b.cfg.StructObjectFields {
		switch named.Kind {
		case ast.Object, ast.Interface, ast.Union:
			return fieldPure
		}
	}
	return fieldResolve
}

type fieldKind int

const (
	fieldPure fieldKind = iota
	fieldResolve
)

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
