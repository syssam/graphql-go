package graphql

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Enum binds the GraphQL enum name to the Go type T using an explicit
// mapping from Go values to SDL enum value names. Every SDL value must be
// mapped exactly once.
func Enum[T comparable](name string, values map[T]string) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Types[name]
		if def == nil || def.Kind != ast.Enum {
			b.errorf("Enum %q: type is not an enum in the schema", name)
			return
		}
		reverse := make(map[string]T, len(values))
		for goVal, sdlName := range values {
			if def.EnumValues.ForName(sdlName) == nil {
				b.errorf("Enum %q: value %q is not defined in the schema", name, sdlName)
				return
			}
			if _, dup := reverse[sdlName]; dup {
				b.errorf("Enum %q: value %q is mapped from more than one Go value", name, sdlName)
				return
			}
			reverse[sdlName] = goVal
		}
		var missing []string
		for _, ev := range def.EnumValues {
			if _, ok := reverse[ev.Name]; !ok {
				missing = append(missing, ev.Name)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			b.errorf("Enum %q: values %s have no Go mapping", name, strings.Join(missing, ", "))
			return
		}
		registerLeaf(b.reg, name, ast.Enum, func(w *jsonw.Writer, v T) error {
			s, ok := values[v]
			if !ok {
				return fmt.Errorf("enum %s cannot represent value: %v", name, v)
			}
			w.String(s)
			return nil
		}, func(raw any) (T, error) {
			s, ok := raw.(string)
			if !ok {
				var zero T
				return zero, fmt.Errorf("enum %s cannot represent non-string value: %s", name, describeRaw(raw))
			}
			v, ok := reverse[s]
			if !ok {
				var zero T
				return zero, fmt.Errorf("value %q does not exist in %s enum", s, name)
			}
			return v, nil
		})
	})
}
