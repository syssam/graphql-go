package codegen

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2/ast"
)

// checkNameCollisions refuses a schema whose generated names would collide.
//
// A Resolver method is its type name and field name run together, so two
// different fields can produce one name: FeatureFlag.targetingRules and
// FeatureFlagTargeting.rules are both FeatureFlagTargetingRules. Without this
// the generator emitted an interface with the method twice and the failure
// arrived as a compile error inside a file marked DO NOT EDIT, naming a line
// the author cannot act on.
//
// The interface and the args structs are per group, so a collision is only
// ever within one, and the cheapest fix is usually to move one of the two
// types to another SDL file rather than to rename a field.
func (b *builder) checkNameCollisions(groups []string) error {
	var errs []error
	report := func(kind, group, name, a, bcoord string) {
		errs = append(errs, fmt.Errorf(
			"codegen: %s in group %q: %s and %s both generate %q; "+
				"move one type to a different SDL file, or rename one of the fields",
			kind, group, a, bcoord, name))
	}
	for _, g := range groups {
		methods := map[string]string{}
		args := map[string]string{}
		b.eachResolverField(g, func(typeName string, fd *ast.FieldDefinition) {
			coord := typeName + "." + fd.Name
			if m := b.resolverMethod(typeName, fd.Name); methods[m] != "" {
				report("resolver method", g, m, methods[m], coord)
			} else {
				methods[m] = coord
			}
			if len(fd.Arguments) == 0 {
				return
			}
			if a := b.argsName(typeName, fd.Name); args[a] != "" {
				report("args struct", g, a, args[a], coord)
			} else {
				args[a] = coord
			}
		})
	}
	return errors.Join(errs...)
}

// checkEnumConstants refuses an enum whose constant has the name of a type
// generated into the same model package. A constant is named Type+Value, which
// is a legal type name too: enum Color { RED } beside type ColorRed declared
// ColorRed twice.
func (b *builder) checkEnumConstants() error {
	pkg := func(name string) string {
		if b.modelSplit {
			return b.groupOf(name)
		}
		return ""
	}
	types := map[[2]string]bool{}
	for name, def := range b.schema.Types {
		if !def.BuiltIn && !b.mapped(name) && !b.isRoot(name) {
			types[[2]string{pkg(name), name}] = true
		}
	}
	var errs []error
	for _, name := range b.typeNames(ast.Enum) {
		if b.mapped(name) {
			continue
		}
		for _, ev := range b.schema.Types[name].EnumValues {
			if ident := name + goIdent(strings.ToLower(ev.Name)); types[[2]string{pkg(name), ident}] {
				errs = append(errs, fmt.Errorf(
					"codegen: enum value %s.%s and type %s both generate the Go name %q; rename one of them in the schema",
					name, ev.Name, ident, ident))
			}
		}
	}
	return errors.Join(errs...)
}

// checkIdentifiers refuses legal SDL whose generated Go would not compile.
// GraphQL names are case-sensitive and Go identifiers are derived from them
// by goIdent, which folds: id and ID are both ID, userId and userID both
// UserID, and the enum values Low and LOW both end in Low. A type is emitted
// under its SDL name, so a type named in lower case is unexported and the
// bindings cannot name it. Each of these used to be written out and fail as a
// compile error in a DO NOT EDIT file; renaming them silently instead would
// make the Go name unguessable from the schema.
func (b *builder) checkIdentifiers() error {
	var errs []error
	fold := func(kind string) func(ident, coord string) {
		seen := map[string]string{}
		return func(ident, coord string) {
			if prev, ok := seen[ident]; ok {
				errs = append(errs, fmt.Errorf(
					"codegen: %s %s and %s both generate the Go name %q; rename one of them in the schema",
					kind, prev, coord, ident))
				return
			}
			seen[ident] = coord
		}
	}
	exported := func(name string) {
		if r, _ := utf8.DecodeRuneInString(name); !unicode.IsUpper(r) {
			errs = append(errs, fmt.Errorf(
				"codegen: type %q would generate an unexported Go type; rename it, or bind it to an existing Go type with Config.Models",
				name))
		}
	}

	for _, name := range b.typeNames(ast.Enum) {
		if b.mapped(name) {
			continue
		}
		exported(name)
		add := fold("enum values")
		for _, ev := range b.schema.Types[name].EnumValues {
			add(name+goIdent(strings.ToLower(ev.Name)), name+"."+ev.Name)
		}
	}
	for _, name := range b.typeNames(ast.Scalar) {
		if !b.mapped(name) {
			exported(name)
		}
	}
	for _, name := range b.typeNames(ast.Object) {
		def := b.schema.Types[name]
		if !b.isRoot(name) && !b.mapped(name) {
			exported(name)
			add := fold("fields")
			for _, fd := range def.Fields {
				if !strings.HasPrefix(fd.Name, "__") && b.fieldKind(name, fd) == fieldPure {
					add(b.fieldIdent(name, fd.Name), name+"."+fd.Name)
				}
			}
		}
		for _, fd := range def.Fields {
			if strings.HasPrefix(fd.Name, "__") || len(fd.Arguments) == 0 {
				continue
			}
			add := fold("arguments")
			for _, arg := range fd.Arguments {
				add(b.argIdent(name, fd.Name, arg.Name), name+"."+fd.Name+"("+arg.Name+":)")
			}
		}
	}
	for _, name := range b.typeNames(ast.InputObject) {
		if b.mapped(name) {
			continue
		}
		exported(name)
		add := fold("input fields")
		for _, fd := range b.schema.Types[name].Fields {
			add(b.fieldIdent(name, fd.Name), name+"."+fd.Name)
		}
	}
	return errors.Join(errs...)
}
