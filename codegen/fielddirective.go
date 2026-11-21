package codegen

import (
	"errors"
	"fmt"
	"go/token"
	"maps"
	"slices"
	"strings"

	"github.com/syssam/graphql-go/internal/gqlname"
	"github.com/vektah/gqlparser/v2/ast"
)

// foldFieldDirective reads every use of Config.FieldDirective once, before
// discovery, and records the Go names its name argument gives.
//
// A name is folded into FieldNames under the field's schema coordinate, so a
// model or input struct, an args struct and a Resolver method all follow it
// through fieldIdent, as gqlgen's GoFieldName did; a coordinate the config
// already names wins, as Models wins over the model directive. It is also kept
// in directiveNames, because on a bound model the author is saying which Go
// field answers, and discovery must look that field up instead of matching the
// SDL name (directiveGoName). A FieldNames entry renames and binds nothing.
//
// An argument the configuration does not read is refused with the field's
// coordinate. It used to be dropped, which left @goField(name: "X") deriving
// another name and the field falling to a Resolver nobody asked for.
func (b *builder) foldFieldDirective() error {
	fdir := b.cfg.FieldDirective
	if fdir.IsZero() {
		return nil
	}
	read := map[string]bool{}
	for _, a := range []string{fdir.ForceResolverArg, fdir.NameArg} {
		if a != "" {
			read[a] = true
		}
	}
	var names map[string]string
	for _, typeName := range slices.Sorted(maps.Keys(b.schema.Types)) {
		def := b.schema.Types[typeName]
		if def.BuiltIn || strings.HasPrefix(typeName, "__") {
			continue
		}
		for _, fd := range def.Fields {
			d := fd.Directives.ForName(fdir.Name)
			if d == nil {
				continue
			}
			coord := typeName + "." + fd.Name
			for _, arg := range d.Arguments {
				if !read[arg.Name] {
					return b.diag(arg.Position, coord, "codegen: %s: @%s(%s:) is not read: %s", coord, fdir.Name, arg.Name, unreadFieldDirectiveArg(fdir, arg.Name))
				}
			}
			if fdir.NameArg == "" {
				continue
			}
			arg := d.Arguments.ForName(fdir.NameArg)
			if arg == nil {
				continue
			}
			if !isStringValue(arg.Value) {
				return b.diag(arg.Position, coord, "codegen: %s: @%s(%s:) must be a string, the Go name", coord, fdir.Name, fdir.NameArg)
			}
			if g := arg.Value.Raw; !token.IsIdentifier(g) || !token.IsExported(g) {
				return b.diag(arg.Position, coord, "codegen: %s: @%s(%s: %q) is not an exported Go identifier", coord, fdir.Name, fdir.NameArg, g)
			}
			if names == nil {
				names = map[string]string{}
			}
			names[coord] = arg.Value.Raw
		}
	}
	if len(names) == 0 {
		return nil
	}
	merged := maps.Clone(b.cfg.FieldNames)
	if merged == nil {
		merged = map[string]string{}
	}
	for coord, g := range names {
		if _, set := merged[coord]; !set {
			merged[coord] = g
		}
	}
	b.cfg.FieldNames = merged
	b.directiveNames = names
	return nil
}

// unreadFieldDirectiveArg says what to do about an argument gqlc does not
// read, naming the option that does it where there is one.
func unreadFieldDirectiveArg(fdir FieldDirective, arg string) string {
	switch arg {
	case "name":
		return "set FieldDirective.NameArg (gqlc.yaml: fieldDirective.nameArg) to bind the field to that Go name"
	case "forceResolver":
		return "set FieldDirective.ForceResolverArg (gqlc.yaml: fieldDirective.forceResolverArg) to honour it"
	case "omittable":
		return "Config.NullableInputOmittable (gqlc.yaml: nullableInputOmittable) makes every nullable input field Omittable; remove the argument"
	}
	return "remove it, or name it in Config.FieldDirective"
}

// checkDirectiveNameTargets refuses a name argument where it would bind
// nothing, once the model map is final, naming the coordinate and what does
// work there.
//
// On an input object bound to Go, Input[T] matches a field by its graphql or
// json tag or by its Go field's name, and gqlc writes nothing for the input
// but that call -- naming one field explicitly (InputField) would mean naming
// every field, with Go types gqlc has not loaded. So a name the engine's own
// derivation does not turn back into the SDL name was accepted and then
// failed NewSchema with "input field PlanIn.maxGb has no binding". One it does
// turn back (AuthorID for authorId) binds as written, and stays.
//
// On an interface no Go field or method is chosen at all: each implementing
// object's field answers, and the name there is the one that counts.
func (b *builder) checkDirectiveNameTargets() error {
	var errs []error
	for _, coord := range slices.Sorted(maps.Keys(b.directiveNames)) {
		goName := b.directiveNames[coord]
		typeName, field, _ := strings.Cut(coord, ".")
		def := b.schema.Types[typeName]
		if def == nil {
			continue
		}
		at := fmt.Sprintf("%s: @%s(%s: %q)", coord, b.cfg.FieldDirective.Name, b.cfg.FieldDirective.NameArg, goName)
		var pos *ast.Position
		if fd := def.Fields.ForName(field); fd != nil {
			pos = fd.Position
			if d := fd.Directives.ForName(b.cfg.FieldDirective.Name); d != nil {
				pos = d.Position
			}
		}
		switch {
		case def.Kind == ast.InputObject && b.mapped(typeName) && inputFieldName(goName) != field:
			errs = append(errs, b.diag(pos, coord, "codegen: %s binds nothing: %s is bound to %s, and Input[T] matches an input field by its graphql or json tag or by its Go field's name (%s reads as %s). Tag the Go field `graphql:%q` and remove the argument",
				at, typeName, b.cfg.Models[typeName], goName, inputFieldName(goName), field))
		case def.Kind == ast.Interface:
			var impls []string
			for _, obj := range b.schema.PossibleTypes[typeName] {
				if obj.Kind == ast.Object && obj.Fields.ForName(field) != nil {
					impls = append(impls, obj.Name+"."+field)
				}
			}
			slices.Sort(impls)
			errs = append(errs, b.diag(pos, coord, "codegen: %s binds nothing: an interface field is answered by each implementing object's field, and %s has no Go field or method of its own. Put the argument on %s",
				at, typeName, summarize(impls, 8)))
		}
	}
	return errors.Join(errs...)
}

// inputFieldName is the GraphQL name Input[T] derives from a Go field's name
// when no tag gives one -- the engine's own rule, not a copy of it.
func inputFieldName(goName string) string { return gqlname.FromGo(goName) }

// directiveGoName is the Go name Config.FieldDirective's name argument gives a
// field, or "" when it gives none.
func (b *builder) directiveGoName(typeName, field string) string {
	return b.directiveNames[typeName+"."+field]
}
