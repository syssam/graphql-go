package graphql

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// enumValueList renders the enum values an error is about, sorted, as the
// subject of its sentence: `value "A" is`, or `values "A", "B" are`.
func enumValueList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)
	names = slices.Compact(names)
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	if len(quoted) == 1 {
		return "value " + quoted[0] + " is"
	}
	return "values " + strings.Join(quoted, ", ") + " are"
}

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
		// Collected and sorted rather than reported as met: values is a map,
		// so the first bad entry differs from run to run, and naming one of
		// several sends the author round the build once per mistake.
		var undefined, repeated []string
		for goVal, sdlName := range values {
			if def.EnumValues.ForName(sdlName) == nil {
				undefined = append(undefined, sdlName)
				continue
			}
			if _, dup := reverse[sdlName]; dup {
				repeated = append(repeated, sdlName)
				continue
			}
			reverse[sdlName] = goVal
		}
		if msg := enumValueList(undefined); msg != "" {
			b.errorf("Enum %q: %s not defined in the schema", name, msg)
		}
		if msg := enumValueList(repeated); msg != "" {
			b.errorf("Enum %q: %s mapped from more than one Go value", name, msg)
		}
		if len(undefined) > 0 || len(repeated) > 0 {
			return
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
		if !b.claimLeaf("Enum", name, reflect.TypeFor[T]()) {
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
