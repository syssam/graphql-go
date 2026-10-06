package codegen

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// checkListDepth refuses a position whose list nesting the engine has no
// binding for: a scalar or enum through more than two list levels, an input
// object through more than one. The Go type generated for it compiles and
// NewSchema then cannot bind it, and no model the author substitutes helps,
// because the limit belongs to the position. An object, interface or union in
// an output position is bound at any depth and is not checked.
func (b *builder) checkListDepth() error {
	var errs []error
	check := func(coord string, t *ast.Type) {
		depth := 0
		for ; t.Elem != nil; t = t.Elem {
			depth++
		}
		def := b.schema.Types[t.NamedType]
		if def == nil {
			return
		}
		limit, what := 0, ""
		switch def.Kind {
		case ast.Scalar, ast.Enum:
			limit, what = 2, "a scalar or enum through two list levels"
		case ast.InputObject:
			limit, what = 1, "an input object through one list level"
		default:
			return
		}
		if depth > limit {
			errs = append(errs, fmt.Errorf("codegen: %s nests %s in %d lists, and the engine binds %s; flatten it, or wrap the inner list in a type of its own", coord, t.NamedType, depth, what))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(b.schema.Types)) {
		def := b.schema.Types[name]
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		for _, fd := range def.Fields {
			check(name+"."+fd.Name, fd.Type)
			for _, arg := range fd.Arguments {
				check(name+"."+fd.Name+"("+arg.Name+":)", arg.Type)
			}
		}
	}
	return errors.Join(errs...)
}
