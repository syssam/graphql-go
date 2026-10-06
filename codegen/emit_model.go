package codegen

import (
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// emitModel renders the model types of one group, or of the whole schema
// when group is empty.
func (b *builder) emitModel(group string) string {
	selfPkg := "model"
	if group != "" {
		selfPkg = group
	}
	var body strings.Builder
	for _, name := range b.typeNames(ast.Enum) {
		if !b.inGroup(name, group) || b.mapped(name) {
			continue
		}
		def := b.schema.Types[name]
		body.WriteString("type " + name + " string\n\nconst (\n")
		for _, ev := range def.EnumValues {
			body.WriteString("\t" + name + goIdent(strings.ToLower(ev.Name)) + " " + name + " = \"" + ev.Name + "\"\n")
		}
		body.WriteString(")\n\n")
	}
	for _, name := range b.typeNames(ast.Scalar) {
		if _, ok := b.cfg.Models[name]; ok {
			continue
		}
		if !b.inGroup(name, group) {
			continue
		}
		body.WriteString("// " + name + " is a custom scalar; bind it with graphql.Scalar in NewSchema options.\n")
		body.WriteString("type " + name + " string\n\n")
	}
	for _, kind := range []ast.DefinitionKind{ast.Interface, ast.Union} {
		for _, name := range b.typeNames(kind) {
			if !b.inGroup(name, group) || !b.hasMarker(name) {
				continue
			}
			body.WriteString("type " + name + " interface {\n")
			for _, m := range b.markerMethods(name) {
				body.WriteString("\t" + m + "()\n")
			}
			body.WriteString("}\n\n")
		}
	}
	for _, name := range b.typeNames(ast.Object) {
		if b.isRoot(name) || !b.inGroup(name, group) || b.mapped(name) {
			continue
		}
		def := b.schema.Types[name]
		body.WriteString("type " + name + " struct {\n")
		for _, fd := range def.Fields {
			if strings.HasPrefix(fd.Name, "__") {
				continue
			}
			if b.fieldKind(name, fd) != fieldPure {
				continue
			}
			body.WriteString("\t" + b.fieldIdent(name, fd.Name) + " " + b.goType(fd.Type, selfPkg, false))
			if jt := b.jsonTag(fd); jt != "" {
				body.WriteString(" `" + strings.TrimPrefix(jt, " ") + "`")
			}
			body.WriteString("\n")
		}
		body.WriteString("}\n\n")
		for _, abstract := range b.markersOf(name) {
			body.WriteString("func (*" + name + ") " + b.markerMethod(abstract) + "() {}\n\n")
		}
	}
	for _, name := range b.typeNames(ast.InputObject) {
		if !b.inGroup(name, group) || b.mapped(name) {
			continue
		}
		def := b.schema.Types[name]
		body.WriteString("type " + name + " struct {\n")
		for _, fd := range def.Fields {
			// The tag pins the SDL name. Deriving it back from the Go name is
			// lossy: an SDL field named ownerID becomes OwnerID, which derives
			// to ownerId, and the binding then fails to match the schema.
			// Validated when the builder was made, so the error cannot occur here.
			extra, _ := b.tagsFor(fd)
			body.WriteString("\t" + b.fieldIdent(name, fd.Name) + " " +
				b.goType(fd.Type, selfPkg, b.cfg.NullableInputOmittable) +
				" `graphql:\"" + fd.Name + "\"" + b.jsonTag(fd) + extra + "`\n")
		}
		body.WriteString("}\n\n")
	}
	s := body.String()
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return b.header(selfPkg, "holds the types generated from the GraphQL schema.") + b.importBlock(s, selfPkg) + s
}
