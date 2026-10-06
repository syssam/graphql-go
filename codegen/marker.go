package codegen

import (
	"unicode"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2/ast"
)

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
			if b.fieldKind(member.Name, fd) == fieldPure && b.fieldIdent(member.Name, fd.Name) == method {
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

// abstractRef is the Go type an interface or union is bound to: its marker
// when it has one. A mapped abstract type stays bound to any, as it always
// was: the executor resolves it from the dynamic type either way.
func (b *builder) abstractRef(name string) string {
	if b.hasMarker(name) {
		return b.modelRef(name, name)
	}
	return "any"
}

// markersOf lists the abstract types with a marker that object belongs to, in
// name order so the output is stable.
func (b *builder) markersOf(object string) []string {
	var out []string
	for _, kind := range []ast.DefinitionKind{ast.Interface, ast.Union} {
		for _, name := range b.typeNames(kind) {
			if !b.hasMarker(name) {
				continue
			}
			for _, member := range b.schema.PossibleTypes[name] {
				if member.Name == object {
					out = append(out, name)
					break
				}
			}
		}
	}
	return out
}
