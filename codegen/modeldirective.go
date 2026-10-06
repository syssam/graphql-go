package codegen

import (
	"maps"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// foldModelDirective reads a type-binding directive off the schema into the
// Models map. An entry already in Models is left alone: a config file is a
// deliberate override of what the SDL happens to say.
//
// A directive with no argument, or one whose argument is not a string, is
// skipped rather than reported. gqlgen's @goModel also accepts models: [..]
// and forceGenerate:, and a schema that uses those forms on a type simply does
// not get a binding from here -- it can still be named in Models.
func foldModelDirective(sch *ast.Schema, d ModelDirective, models map[string]string) map[string]string {
	out := make(map[string]string, len(models)+len(sch.Types))
	maps.Copy(out, models)
	names := make([]string, 0, len(sch.Types))
	for name := range sch.Types {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		def := sch.Types[name]
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		dir := def.Directives.ForName(d.Name)
		if dir == nil {
			continue
		}
		arg := dir.Arguments.ForName(d.Arg)
		if arg == nil || arg.Value == nil || arg.Value.Kind != ast.StringValue {
			continue
		}
		if expr := strings.TrimSpace(arg.Value.Raw); expr != "" {
			out[name] = expr
		}
	}
	return out
}
