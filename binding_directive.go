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

// Directive binds a schema directive applied to field definitions. For every
// field carrying @name, fn receives the field's executor and the decoded
// directive arguments and returns the wrapped executor. Use struct{} for A
// when the directive has no arguments; otherwise register Args[A].
func Directive[A any](name string, fn func(next FieldFunc, args A) FieldFunc) SchemaOption {
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
						v, err := a.Value.Value(nil)
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

// applyDirectives wraps every field executor with the bound directives
// present on its definition, outermost first in SDL order.
func (b *schemaBuilder) applyDirectives(s *Schema) {
	for _, obj := range s.objects {
		for _, fd := range obj.fields {
			for i := len(fd.def.Directives) - 1; i >= 0; i-- {
				d := fd.def.Directives[i]
				db := b.directives[d.Name]
				if db == nil {
					if !builtinDirectives[d.Name] {
						slog.Debug("graphql: directive has no binding and is ignored", "directive", d.Name, "field", coordinate(obj.name, fd.name))
					}
					continue
				}
				coord := coordinate(obj.name, fd.name)
				wrapper, err := db.wrap(b, d, b.ast.Directives[d.Name], coord)
				if err != nil {
					b.errs = append(b.errs, fmt.Errorf("graphql: %w", err))
					continue
				}
				fd.wrap(wrapper)
			}
		}
	}
}
