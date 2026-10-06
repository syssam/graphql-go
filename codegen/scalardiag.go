package codegen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// reportUnboundScalars names every custom scalar the author still has to bind.
//
// A Models entry chooses the Go type; it does not register a marshaller, so a
// mapped scalar needs graphql.Scalar exactly as an unmapped one does. Believing
// otherwise is what made 21 641 of one real schema's build errors a surprise:
// Time was mapped to time.Time and still had no binding. The generator knows
// the whole list while it is generating, so it says so then rather than letting
// NewSchema say it later, once per use.
//
// Each is named when it is new and not again. Nothing here can see NewSchema,
// so a scalar bound long ago reads the same as one that is not, and naming
// all of them on every generate was seven lines of one real schema's output
// that had been true once: the scalar added that day was the eighth name in a
// list nobody read. What has been named is written into schema.go, in the
// comment on sdl, and the last generate's list is what this one leaves out.
func (b *builder) reportUnboundScalars() {
	names := b.typeNames(ast.Scalar)
	if len(names) == 0 {
		return
	}
	parts := make([]string, 0, len(names))
	custom := ""
	for _, name := range names {
		if b.marshalers[name] {
			continue
		}
		b.unboundScalars = append(b.unboundScalars, name)
		if slices.Contains(b.scalarsNamedBefore, name) {
			continue
		}
		goType := "model." + name
		if expr, ok := b.cfg.Models[name]; ok {
			_, ref := splitModelExpr(expr)
			goType = ref
		}
		if goType == "time.Time" {
			// The engine has this one's binding, so its fix is one call.
			parts = append(parts, fmt.Sprintf("%s (time.Time: graphql.Time(%q))", name, name))
			continue
		}
		if custom == "" {
			custom = name
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", name, goType))
	}
	if len(parts) == 0 {
		return
	}
	how := "Add the call shown for each"
	if custom != "" {
		how = fmt.Sprintf("Add graphql.Scalar[GoType](%q, marshal, unmarshal) for each, or the call shown", custom)
	}
	b.notef("%d custom scalar(s) need a binding at NewSchema, or the schema will not build: %s. %s; a Models entry chooses the Go type but does not bind it.",
		len(parts), strings.Join(parts, ", "), how)
}
