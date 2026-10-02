package graphql

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// FieldFunc is the type-erased field executor seen by directives and field
// interceptors. args is the decoded argument struct pointer (*A) or nil for
// fields without a bound argument type.
type FieldFunc func(ctx context.Context, parent any, args any) (any, error)

type directiveBinding struct {
	name     string
	argsType reflect.Type
	wrap     func(b *schemaBuilder, d *ast.Directive, def *ast.DirectiveDefinition, coord string) (func(FieldFunc) FieldFunc, error)
}

// Directive binds a schema directive that has no arguments. For every field
// (or every field of an object) carrying @name, fn wraps the field executor.
// Directives with arguments use DirectiveArgs.
func Directive(name string, fn func(next FieldFunc) FieldFunc) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Directives[name]
		if def != nil && len(def.Arguments) > 0 {
			b.errorf("Directive %q: has arguments; use DirectiveArgs", name)
			return
		}
		DirectiveArgs(name, func(next FieldFunc, _ struct{}) FieldFunc { return fn(next) }).applySchema(b)
	})
}

// SubscriptionRootDirective declares that the named directives, bound with Directive or
// DirectiveArgs for ordinary fields, are enforced on subscription root fields by a
// SubscriptionInterceptor the application installs.
//
// A bound directive cannot wrap a subscription root field (the field is served by its stream,
// not its resolver), so a schema that carries one there is refused rather than built with a
// check that never runs. An application that enforces the directive itself when a subscription
// opens says so here, and the refusal is lifted for exactly those names. Naming a directive
// that is not bound is an error: a typo must not switch the refusal off for nothing.
func SubscriptionRootDirective(names ...string) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		for _, n := range names {
			b.subDirectives[n] = true
		}
	})
}

// DirectiveArgs binds a schema directive whose arguments decode into A.
// Register Args[A] when A is not struct{}. Locations FIELD_DEFINITION and
// OBJECT are applied; an OBJECT directive wraps every field of that type. An
// interface's field has no executor to wrap, so a bound directive written
// there must also be written on each implementing object's field, and the
// build fails when it is not.
func DirectiveArgs[A any](name string, fn func(next FieldFunc, args A) FieldFunc) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Directives[name]
		if def == nil {
			b.errorf("Directive %q: directive is not defined in the schema", name)
			return
		}
		if _, dup := b.directives[name]; dup {
			b.errorf("Directive %q: bound more than once", name)
			return
		}
		tA := reflect.TypeFor[A]()
		b.directives[name] = &directiveBinding{
			name:     name,
			argsType: tA,
			wrap: func(b *schemaBuilder, d *ast.Directive, def *ast.DirectiveDefinition, coord string) (func(FieldFunc) FieldFunc, error) {
				var args A
				if len(def.Arguments) > 0 {
					tmpl := b.reg.argsDecoders[tA]
					if tmpl == nil {
						return nil, fmt.Errorf("directive @%s: no Args[%s] registered for its arguments", name, tA)
					}
					dec, err := tmpl.build(b, def.Arguments, "@"+name)
					if err != nil {
						return nil, err
					}
					raw := make(map[string]any, len(d.Arguments))
					for _, a := range d.Arguments {
						v, err := astJSON(a.Value, nil)
						if err != nil {
							return nil, fmt.Errorf("directive @%s on %s: argument %s: %w", name, coord, a.Name, err)
						}
						raw[a.Name] = v
					}
					decoded, err := dec.decode(raw)
					if err != nil {
						return nil, fmt.Errorf("directive @%s on %s: %w", name, coord, err)
					}
					args = *decoded.(*A)
				}
				return func(next FieldFunc) FieldFunc { return fn(next, args) }, nil
			},
		}
	})
}

// builtinDirectives are defined by the specification and never wrap
// executors.
var builtinDirectives = map[string]bool{"deprecated": true, "specifiedBy": true, "skip": true, "include": true, "oneOf": true}

// locationList renders a directive's declared locations for an error message.
func locationList(locs []ast.DirectiveLocation) string {
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		out = append(out, string(l))
	}
	return strings.Join(out, " | ")
}

func supportedDirectiveLocation(locs []ast.DirectiveLocation) bool {
	for _, loc := range locs {
		if loc == ast.LocationFieldDefinition || loc == ast.LocationObject {
			return true
		}
	}
	return false
}

func (b *schemaBuilder) wrapWithDirective(_ *Schema, fd *fieldDef, d *ast.Directive, coord string) {
	db := b.directives[d.Name]
	if db == nil {
		if !builtinDirectives[d.Name] {
			slog.Debug("graphql: directive has no binding and is ignored", "directive", d.Name, "field", coord)
		}
		return
	}
	wrapper, err := db.wrap(b, d, b.ast.Directives[d.Name], coord)
	if err != nil {
		b.errs = append(b.errs, fmt.Errorf("graphql: %w", err))
		return
	}
	fd.wrap(wrapper)
}

// applyDirectives wraps every field executor with the bound directives
// present on its definition and on its object type. Field directives are
// inner; object directives are outer, so @auth on a type wraps field logic.
func (b *schemaBuilder) applyDirectives(s *Schema) {
	// A binding whose directive can be applied nowhere this engine wraps is a
	// build error, not a debug line. FIELD_DEFINITION and OBJECT are the only
	// locations DirectiveArgs applies, so a directive declared on neither can
	// never run: the author has written a wrapper, the schema builds, and
	// nothing tells them it is dead. That is the fail-open the requirement
	// directives already reject misplacement to prevent, and the adjacent
	// case -- a binding for a directive the SDL does not declare at all -- is
	// already an error two functions up.
	for _, db := range b.directives {
		def := b.ast.Directives[db.name]
		if def != nil && !supportedDirectiveLocation(def.Locations) {
			b.errorf("Directive %q: declared on %s, so it can never wrap a field; DirectiveArgs applies FIELD_DEFINITION and OBJECT",
				db.name, locationList(def.Locations))
		}
	}
	for name := range b.subDirectives {
		if b.directives[name] == nil {
			b.errorf("SubscriptionRootDirective(%q): no directive of that name is bound", name)
		}
	}
	for _, obj := range s.objects {
		subRoot := b.ast.Subscription != nil && obj.name == b.ast.Subscription.Name
		for _, fd := range obj.fields {
			if subRoot {
				b.rejectDirectivesOnSubscriptionRoot(obj, fd)
				continue
			}
			b.rejectDirectivesLeftOnTheInterface(obj, fd)
			for i := len(fd.def.Directives) - 1; i >= 0; i-- {
				b.wrapWithDirective(s, fd, fd.def.Directives[i], coordinate(obj.name, fd.name))
			}
			for i := len(obj.def.Directives) - 1; i >= 0; i-- {
				b.wrapWithDirective(s, fd, obj.def.Directives[i], obj.name)
			}
		}
	}
}

// rejectDirectivesLeftOnTheInterface fails the build for a bound directive
// written on an interface's field and not on the field of an object
// implementing it. FIELD_DEFINITION is a location this engine applies, and an
// interface field is written at it, but only an object's field has an executor
// to wrap: the directive on Node.secret wrapped nothing, and User.secret was
// served without it. Repeating it on the object's field, or on the object,
// is what runs, and a schema that does so builds.
func (b *schemaBuilder) rejectDirectivesLeftOnTheInterface(obj *objectType, fd *fieldDef) {
	for _, name := range obj.def.Interfaces {
		iface := b.ast.Types[name]
		if iface == nil {
			continue
		}
		ifd := iface.Fields.ForName(fd.name)
		if ifd == nil {
			continue
		}
		for _, d := range ifd.Directives {
			if b.directives[d.Name] == nil || fd.def.Directives.ForName(d.Name) != nil || obj.def.Directives.ForName(d.Name) != nil {
				continue
			}
			b.errorf("field %s: @%s is bound and written on %s, but a directive on an interface field never runs; write it on %s as well",
				coordinate(obj.name, fd.name), d.Name, coordinate(name, fd.name), coordinate(obj.name, fd.name))
		}
	}
}

// rejectDirectivesOnSubscriptionRoot fails the build for a bound directive on
// a subscription root field. Wrapping fd.anyResolve there has no effect: the
// field is served by fd.subscribe, and the per-event writer substitutes its
// executor outright. Reporting it beats a check that quietly never runs.
func (b *schemaBuilder) rejectDirectivesOnSubscriptionRoot(obj *objectType, fd *fieldDef) {
	for _, d := range append(append([]*ast.Directive(nil), fd.def.Directives...), obj.def.Directives...) {
		if b.directives[d.Name] == nil || b.subDirectives[d.Name] {
			continue
		}
		b.errorf("field %s: @%s is bound, but a directive on a subscription root field never runs; use a SubscriptionInterceptor (and declare it with SubscriptionRootDirective)", coordinate(obj.name, fd.name), d.Name)
	}
}
