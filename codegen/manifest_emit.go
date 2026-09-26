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
	// A value bound at a nullable position is emitted returning the value
	// type; the engine writes a present value for a field that may be null.
	if fb.Value && strings.HasPrefix(goRet, "*") {
		goRet = goRet[1:]
	}
	name := fb.goName(fd.Name)
	// wrap converts the value to the type the field needs, for a binding whose
	// Go side is a different named type over the same basic kind.
	wrap := func(expr string) string {
		if !fb.Convert {
			return expr
		}
		return goRet + "(" + expr + ")"
	}

	if fb.Kind == FieldStruct {
		return fmt.Sprintf("\t\t\tgraphql.Field(%q, func(v %s) %s { return %s }),\n",
			fd.Name, recv, goRet, wrap("v."+name))
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
			return fmt.Sprintf("\t\t\tgraphql.FieldArgs(%q, func(v %s, a %s) %s { return %s }),\n",
				fd.Name, recv, b.argsName(typeName, fd.Name), goRet,
				wrap(fmt.Sprintf("v.%s(%s)", name, spread)))
		}
		return fmt.Sprintf("\t\t\tgraphql.Field(%q, func(v %s) %s { return %s }),\n",
			fd.Name, recv, goRet, wrap("v."+name+"()"))
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
		// Listed in Config.Inline or not; InlineAccessors never reaches a
		// method with arguments.
		return fmt.Sprintf("\t\t\tgraphql.ResolveArgs(%q, func(%s, v %s, a %s) (%s, error) { return %s }%s),\n",
			fd.Name, ctxParam, recv, b.argsName(typeName, fd.Name), goRet, call, b.schedule(typeName, fd.Name))
	}
	schedule := b.schedule(typeName, fd.Name)
	if b.cfg.InlineAccessors {
		schedule = ", graphql.Inline()"
	}
	return fmt.Sprintf("\t\t\tgraphql.Resolve(%q, func(%s, v %s) (%s, error) { return %s }%s),\n",
		fd.Name, ctxParam, recv, goRet, call, schedule)
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
