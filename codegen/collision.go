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
	type seen struct {
		typeName string
		fd       *ast.FieldDefinition
	}
	coordOf := func(s seen) string { return s.typeName + "." + s.fd.Name }
	report := func(kind, group, name string, first, second seen) {
		at := second
		fix := "move one type to a different SDL file, or rename one of the fields"
		// Two root fields cannot be moved apart -- Query and Mutation are not
		// types an author splits by file to dodge a name -- and renaming one
		// changes the API. Naming the Go identifier -- the method and the args
		// struct both derive from it -- is the fix that changes nothing a
		// client sees, so it is the one spelled out, for the field that is not
		// Query's when one is.
		if b.isRoot(first.typeName) && b.isRoot(second.typeName) {
			if b.schema.Query != nil && second.typeName == b.schema.Query.Name {
				at = first
			}
			ident := at.typeName + b.fieldIdent(at.typeName, at.fd.Name)
			fix = fmt.Sprintf("name one field's Go identifier instead: Config.FieldNames[%q] = %q (gqlc.yaml: fieldNames: {%s: %s})",
				coordOf(at), ident, coordOf(at), ident)
		}
		errs = append(errs, b.diag(at.fd.Position, coordOf(at),
			"codegen: %s in group %q: %s and %s both generate %q; %s",
			kind, group, coordOf(first), coordOf(second), name, fix))
	}
	for _, g := range groups {
		methods := map[string]seen{}
		args := map[string]seen{}
		// Each name is checked over the walk that emits it: a method for each
		// field eachResolverField gives emitResolver, an args struct for every
		// field with arguments emitArgs writes, model-answered ones included.
		// Checked over the Resolver's fields alone, a manifest method on
		// FeatureFlag.targetingRules(x:) beside the resolver
		// FeatureFlagTargeting.rules(x:) wrote FeatureFlagTargetingRulesArgs
		// twice.
		b.eachResolverField(g, func(typeName string, fd *ast.FieldDefinition) {
			cur := seen{typeName, fd}
			if m := b.resolverMethod(typeName, fd.Name); methods[m].fd != nil {
				report("resolver method", g, m, methods[m], cur)
			} else {
				methods[m] = cur
			}
		})
		b.eachGroupField(g, func(typeName string, fd *ast.FieldDefinition) {
			if len(fd.Arguments) == 0 {
				return
			}
			cur := seen{typeName, fd}
			if a := b.argsName(typeName, fd.Name); args[a].fd != nil {
				report("args struct", g, a, args[a], cur)
			} else {
				args[a] = cur
			}
		})
	}
	return errors.Join(errs...)
}

// checkEnumConstants refuses an enum constant whose name another declaration
// in the same model package already has. A constant is named Type+Value, which
// is a legal type name too -- enum Color { RED } beside type ColorRed declared
// ColorRed twice -- and a legal constant of another enum: A._B and AX.B are
// both AXB, since goIdent prefixes an underscored value with X.
//
// The table holds what emitModel declares, by declaresModel, so an abstract
// type that is any and declares nothing collides with nothing.
func (b *builder) checkEnumConstants() error {
	pkg := func(name string) string {
		if b.modelSplit {
			return b.groupOf(name)
		}
		return ""
	}
	type declared struct{ what, enum string }
	names := map[[2]string]declared{}
	for name := range b.schema.Types {
		if b.declaresModel(name) {
			names[[2]string{pkg(name), name}] = declared{what: "type " + name}
		}
	}
	var errs []error
	for _, name := range b.typeNames(ast.Enum) {
		if !b.declaresModel(name) {
			continue
		}
		for _, ev := range b.schema.Types[name].EnumValues {
			ident := name + goIdent(strings.ToLower(ev.Name))
			key := [2]string{pkg(name), ident}
			prev, taken := names[key]
			switch {
			case !taken:
				names[key] = declared{what: "enum value " + name + "." + ev.Name, enum: name}
			case prev.enum == name:
				// Two values of one enum folding together is checkIdentifiers'
				// to report, and it has, before this runs.
			case prev.enum == "":
				errs = append(errs, fmt.Errorf(
					"codegen: enum value %s.%s and type %s both generate the Go name %q; rename one of them in the schema",
					name, ev.Name, ident, ident))
			default:
				errs = append(errs, fmt.Errorf(
					"codegen: %s and enum value %s.%s both generate the Go name %q; rename one of them in the schema",
					prev.what, name, ev.Name, ident))
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
