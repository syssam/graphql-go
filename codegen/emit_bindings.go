package codegen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

func (b *builder) emitBindings(group string, withResolver bool) string {
	var w strings.Builder
	whose := "the schema's"
	if group != "" {
		whose = "the " + group + " group's"
	}
	if withResolver {
		fmt.Fprintf(&w, "// Bindings registers %s types and fields, calling r for each\n", whose)
		w.WriteString("// Resolver method. Building a schema never calls r, which is why\n")
		w.WriteString("// ValidateSchema can pass nil.\n")
		w.WriteString("func Bindings(r Resolver) graphql.SchemaOption {\n\treturn graphql.Options(\n")
	} else {
		fmt.Fprintf(&w, "// Bindings registers %s types and fields; none of them needs a\n", whose)
		w.WriteString("// Resolver, so NewSchema registers them itself.\n")
		w.WriteString("func Bindings() graphql.SchemaOption {\n\treturn graphql.Options(\n")
	}

	for _, name := range b.typeNames(ast.Enum) {
		if !b.inGroup(name, group) {
			continue
		}
		if b.marshalers[name] {
			w.WriteString("\t\tgraphql.EnumMarshaler[" + b.modelRef(name, name) + "](\"" + name + "\"),\n")
			continue
		}
		def := b.schema.Types[name]
		w.WriteString("\t\tgraphql.Enum[" + b.modelRef(name, name) + "](\"" + name + "\", map[" + b.modelRef(name, name) + "]string{\n")
		for _, ev := range def.EnumValues {
			c, declared := b.manifest.enumConstant(name, ev.Name)
			if !declared {
				c = name + goIdent(strings.ToLower(ev.Name))
			}
			w.WriteString("\t\t\t" + b.modelRef(name, c) + ": \"" + ev.Name + "\",\n")
		}
		w.WriteString("\t\t}),\n")
		// A second Go type for the same enum. The registry is keyed on
		// (GraphQL type, reflect.Type), so both bindings live at once and each
		// position decodes into the type it actually holds.
		for _, e := range b.extraEnums[name] {
			w.WriteString("\t\tgraphql.Enum[" + e.ref + "](\"" + name + "\", map[" + e.ref + "]string{\n")
			for _, ev := range def.EnumValues {
				w.WriteString("\t\t\t" + e.consts[ev.Name] + ": \"" + ev.Name + "\",\n")
			}
			w.WriteString("\t\t}),\n")
		}
	}
	// A scalar is bound here only when its type encodes itself; any other
	// custom scalar is the author's to bind, and reportUnboundScalars says so.
	for _, name := range b.typeNames(ast.Scalar) {
		if b.marshalers[name] && b.inGroup(name, group) {
			w.WriteString("\t\tgraphql.ScalarMarshaler[" + b.modelRef(name, name) + "](\"" + name + "\"),\n")
		}
	}
	for _, name := range b.typeNames(ast.InputObject) {
		if !b.inGroup(name, group) {
			continue
		}
		opt := ""
		if b.cfg.ZeroForNullInputs {
			opt = ", graphql.ZeroForNull()"
		}
		w.WriteString("\t\tgraphql.Input[" + b.modelRef(name, name) + "](\"" + name + "\"" + opt + "),\n")
	}
	for _, name := range b.typeNames(ast.Interface) {
		if !b.inGroup(name, group) {
			continue
		}
		w.WriteString("\t\tgraphql.Interface[" + b.abstractRef(name) + "](\"" + name + "\"),\n")
	}
	for _, name := range b.typeNames(ast.Union) {
		if !b.inGroup(name, group) {
			continue
		}
		w.WriteString("\t\tgraphql.Union[" + b.abstractRef(name) + "](\"" + name + "\"),\n")
	}

	var rootFields = map[string][]string{}
	for _, name := range b.typeNames(ast.Object) {
		if b.isRoot(name) {
			for _, fd := range b.schema.Types[name].Fields {
				if strings.HasPrefix(fd.Name, "__") || !b.fieldInGroup(name, fd, group) {
					continue
				}
				rootFields[name] = append(rootFields[name], b.fieldCall(name, fd, true))
			}
			continue
		}
		if !b.inGroup(name, group) {
			continue
		}
		def := b.schema.Types[name]
		w.WriteString("\t\tgraphql.Object[" + b.modelRef(name, name) + "](\"" + name + "\",\n")
		for _, fd := range def.Fields {
			if strings.HasPrefix(fd.Name, "__") {
				continue
			}
			w.WriteString(b.fieldCall(name, fd, false))
		}
		w.WriteString("\t\t),\n")
	}

	// Each group binds only its own root fields; the engine merges the
	// several Query calls this makes across groups into one root.
	for _, root := range []struct {
		def  *ast.Definition
		call string
	}{{b.schema.Query, "Query"}, {b.schema.Mutation, "Mutation"}, {b.schema.Subscription, "Subscription"}} {
		if root.def == nil || len(rootFields[root.def.Name]) == 0 {
			continue
		}
		w.WriteString("\t\tgraphql." + root.call + "(\n")
		for _, c := range rootFields[root.def.Name] {
			w.WriteString(c)
		}
		w.WriteString("\t\t),\n")
	}

	seen := map[string]bool{}
	b.eachGroupField(group, func(name string, fd *ast.FieldDefinition) {
		if len(fd.Arguments) == 0 {
			return
		}
		an := b.argsName(name, fd.Name)
		if seen[an] {
			return
		}
		seen[an] = true
		w.WriteString("\t\tgraphql.Args[" + an + "](),\n")
	})

	w.WriteString("\t)\n}\n")
	return w.String()
}

func (b *builder) fieldCall(typeName string, fd *ast.FieldDefinition, root bool) string {
	goRet := b.goType(fd.Type, "", false)
	if fb, ok := b.modelAnswers(typeName, fd); ok {
		return b.manifestFieldCall(typeName, fd, fb, goRet)
	}
	switch {
	case b.isSubscriptionRoot(typeName):
		meth := b.resolverMethod(typeName, fd.Name)
		if len(fd.Arguments) > 0 {
			an := b.argsName(typeName, fd.Name)
			return fmt.Sprintf("\t\t\tgraphql.SubscribeArgs(%q, func(ctx context.Context, a %s) (<-chan %s, error) { return r.%s(ctx, a) }),\n", fd.Name, an, goRet, meth)
		}
		return fmt.Sprintf("\t\t\tgraphql.Subscribe(%q, func(ctx context.Context) (<-chan %s, error) { return r.%s(ctx) }),\n", fd.Name, goRet, meth)
	case b.fieldKind(typeName, fd) == fieldPure:
		recv := "*" + b.modelRef(typeName, typeName)
		ident := b.fieldIdent(typeName, fd.Name)
		return fmt.Sprintf("\t\t\tgraphql.Field(%q, func(v %s) %s { return v.%s }),\n", fd.Name, recv, goRet, ident)
	case len(fd.Arguments) > 0 && root:
		an := b.argsName(typeName, fd.Name)
		meth := b.resolverMethod(typeName, fd.Name)
		return fmt.Sprintf("\t\t\tgraphql.ResolveArgs(%q, func(ctx context.Context, _ graphql.Root, a %s) (%s, error) { return r.%s(ctx, a) }%s),\n", fd.Name, an, goRet, meth, b.schedule(typeName, fd.Name))
	case len(fd.Arguments) > 0:
		an := b.argsName(typeName, fd.Name)
		meth := b.resolverMethod(typeName, fd.Name)
		return fmt.Sprintf("\t\t\tgraphql.ResolveArgs(%q, func(ctx context.Context, v *%s, a %s) (%s, error) { return r.%s(ctx, v, a) }%s),\n", fd.Name, b.modelRef(typeName, typeName), an, goRet, meth, b.schedule(typeName, fd.Name))
	case root:
		meth := b.resolverMethod(typeName, fd.Name)
		return fmt.Sprintf("\t\t\tgraphql.Resolve(%q, func(ctx context.Context, _ graphql.Root) (%s, error) { return r.%s(ctx) }%s),\n", fd.Name, goRet, meth, b.schedule(typeName, fd.Name))
	default:
		meth := b.resolverMethod(typeName, fd.Name)
		return fmt.Sprintf("\t\t\tgraphql.Resolve(%q, func(ctx context.Context, v *%s) (%s, error) { return r.%s(ctx, v) }%s),\n", fd.Name, b.modelRef(typeName, typeName), goRet, meth, b.schedule(typeName, fd.Name))
	}
}

// schedule is the scheduling option a resolver binding for typeName.field
// carries: graphql.Inline() when Config.Inline names it.
func (b *builder) schedule(typeName, field string) string {
	if slices.Contains(b.cfg.Inline, typeName+"."+field) {
		return ", graphql.Inline()"
	}
	return ""
}

// checkInline refuses a Config.Inline coordinate that names no field, which
// is what a typo produces and would otherwise leave the field concurrent
// while the configuration reads as if it were not.
func (b *builder) checkInline() error {
	for _, coord := range b.cfg.Inline {
		typeName, field, ok := strings.Cut(coord, ".")
		def := b.schema.Types[typeName]
		if !ok || def == nil || def.Fields.ForName(field) == nil {
			return fmt.Errorf("codegen: inline %q names no field", coord)
		}
	}
	return nil
}
