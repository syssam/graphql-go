package codegen

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// manifestFieldCall emits a field the manifest bound explicitly.
//
// The binding carries the Go side's shape rather than the generator looking it
// up, because manifest mode loads no type information — that loading is the
// cost this mode exists to avoid. So the four axes are all declared: struct
// field or method, context or not, error or not, arguments or not.
func (b *builder) manifestFieldCall(typeName string, fd *ast.FieldDefinition, fb FieldBinding, goRet string) string {
	recv := "*" + b.modelRef(typeName, typeName)
	name := fb.goName(fd.Name)

	if fb.Kind == FieldStruct {
		return fmt.Sprintf("\t\t\tgraphql.Field(%q, func(v %s) %s { return v.%s }),\n",
			fd.Name, recv, goRet, name)
	}

	hasArgs := len(fd.Arguments) > 0
	// Arguments are spread into the call rather than passed as the generated
	// struct. Passing the struct would make the bound type's package import
	// the generated package, which already imports it for the model: an
	// import cycle, and an ORM entity depending on GraphQL types. Spread, the
	// method reads the way it would have been written anyway.
	spread := ""
	if hasArgs {
		parts := make([]string, 0, len(fd.Arguments))
		for _, arg := range fd.Arguments {
			parts = append(parts, "a."+goIdent(arg.Name))
		}
		spread = strings.Join(parts, ", ")
	}

	// A pure method needs no context and cannot fail, so it binds with Field
	// and runs inline like struct access.
	if fb.pure() {
		if hasArgs {
			return fmt.Sprintf("\t\t\tgraphql.FieldArgs(%q, func(v %s, a %s) %s { return v.%s(%s) }),\n",
				fd.Name, recv, b.argsName(typeName, fd.Name), goRet, name, spread)
		}
		return fmt.Sprintf("\t\t\tgraphql.Field(%q, func(v %s) %s { return v.%s() }),\n",
			fd.Name, recv, goRet, name)
	}

	// Anything else goes through Resolve, which always supplies a context
	// whether the method wants one or not.
	ctxParam, ctxArg := "_ context.Context", ""
	if fb.Context {
		ctxParam, ctxArg = "ctx context.Context", "ctx"
	}
	call := fmt.Sprintf("v.%s(%s)", name, joinArgs(ctxArg, spread))
	if !fb.Error {
		call += ", nil"
	}

	if hasArgs {
		return fmt.Sprintf("\t\t\tgraphql.ResolveArgs(%q, func(%s, v %s, a %s) (%s, error) { return %s }),\n",
			fd.Name, ctxParam, recv, b.argsName(typeName, fd.Name), goRet, call)
	}
	return fmt.Sprintf("\t\t\tgraphql.Resolve(%q, func(%s, v %s) (%s, error) { return %s }),\n",
		fd.Name, ctxParam, recv, goRet, call)
}

func joinArgs(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ", ")
}
