package graphql

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"

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

// DirectiveArgs binds a schema directive whose arguments decode into A.
// Register Args[A] when A is not struct{}. Locations FIELD_DEFINITION and
// OBJECT are applied; an OBJECT directive wraps every field of that type.
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
	for _, db := range b.directives {
		def := b.ast.Directives[db.name]
		if def != nil && !supportedDirectiveLocation(def.Locations) {
			slog.Debug("graphql: directive binding is ignored for unsupported locations", "directive", db.name)
		}
	}
	for _, obj := range s.objects {
		subRoot := b.ast.Subscription != nil && obj.name == b.ast.Subscription.Name
		for _, fd := range obj.fields {
			if subRoot {
				b.rejectDirectivesOnSubscriptionRoot(obj, fd)
				continue
			}
			for i := len(fd.def.Directives) - 1; i >= 0; i-- {
				b.wrapWithDirective(s, fd, fd.def.Directives[i], coordinate(obj.name, fd.name))
			}
			for i := len(obj.def.Directives) - 1; i >= 0; i-- {
				b.wrapWithDirective(s, fd, obj.def.Directives[i], obj.name)
			}
		}
	}
}

// rejectDirectivesOnSubscriptionRoot fails the build for a bound directive on
// a subscription root field. Wrapping fd.anyResolve there has no effect: the
// field is served by fd.subscribe, and the per-event writer substitutes its
// executor outright. Reporting it beats a check that quietly never runs.
func (b *schemaBuilder) rejectDirectivesOnSubscriptionRoot(obj *objectType, fd *fieldDef) {
	for _, d := range append(append([]*ast.Directive(nil), fd.def.Directives...), obj.def.Directives...) {
		if b.directives[d.Name] == nil {
			continue
		}
		b.errorf("field %s: @%s is bound, but a directive on a subscription root field never runs; use a SubscriptionInterceptor", coordinate(obj.name, fd.name), d.Name)
	}
}
