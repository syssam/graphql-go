package codegen

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

func (b *builder) emitArgs(group string) string {
	var body strings.Builder
	b.eachGroupField(group, func(name string, fd *ast.FieldDefinition) {
		if len(fd.Arguments) == 0 {
			return
		}
		body.WriteString("type " + b.argsName(name, fd.Name) + " struct {\n")
		for _, arg := range fd.Arguments {
			// The tag pins the SDL name, for the reason the input-object
			// emitter above gives: deriving it back from the Go name is
			// lossy. Args structs went without one until an SDL argument
			// arrived that does not survive the round trip -- ownerID
			// derives to ownerId, and _lastUpdatedAt to xLastUpdatedAt.
			body.WriteString("\t" + b.argIdent(name, fd.Name, arg.Name) + " " +
				b.goType(arg.Type, "", false) +
				" `graphql:\"" + arg.Name + "\"`\n")
		}
		body.WriteString("}\n\n")
	})
	return body.String()
}

// eachResolverField calls fn for every field of group that becomes a method on
// its Resolver interface. The collision check and the emitter share it, so the
// two cannot disagree about what the interface contains.
func (b *builder) eachResolverField(group string, fn func(typeName string, fd *ast.FieldDefinition)) {
	b.eachGroupField(group, func(name string, fd *ast.FieldDefinition) {
		if b.fieldKind(name, fd) != fieldResolve {
			return
		}
		// A method binding that can fail is still scheduled like a
		// resolver, but it is answered by the model, so asking the
		// Resolver interface for it would demand a method nothing calls.
		if _, ok := b.modelAnswers(name, fd); ok {
			return
		}
		fn(name, fd)
	})
}

func (b *builder) emitResolver(group string) string {
	var body strings.Builder
	body.WriteString("// Resolver has a method for each field no Go value answers by itself: every\n")
	body.WriteString("// root field, and each field not bound to a struct field or method.\n")
	body.WriteString("type Resolver interface {\n")
	for _, m := range b.resolverSignatures(group, "") {
		// The interface is what is read before a method is written, so it
		// carries the field's SDL description, as a gRPC service interface
		// carries the .proto comments.
		fmt.Fprintf(&body, "\t// %s resolves %s.\n", m.name, m.coord)
		if doc := strings.TrimSpace(m.doc); doc != "" {
			body.WriteString("\t//\n")
			for line := range strings.SplitSeq(doc, "\n") {
				body.WriteString(strings.TrimRight("\t// "+line, " ") + "\n")
			}
		}
		fmt.Fprintf(&body, "\t%s%s\n", m.name, m.signature)
	}
	body.WriteString("}\n")
	return body.String()
}

// resolverSig is one method of a group's Resolver interface.
type resolverSig struct {
	name      string
	signature string // "(ctx context.Context, args XArgs) (T, error)"
	coord     string // "Query.products", for messages and stub comments
	doc       string // the field's SDL description
}

// resolverSignatures renders the group's Resolver methods. The interface
// and the scaffolded stubs both come from here, so a stub always has the
// signature the interface asks for. argsQual qualifies the args structs,
// which live in the group package: empty inside it, "productgql." outside.
func (b *builder) resolverSignatures(group, argsQual string) []resolverSig {
	var out []resolverSig
	b.eachResolverField(group, func(name string, fd *ast.FieldDefinition) {
		ret := b.goType(fd.Type, "", false)
		args := ""
		if len(fd.Arguments) > 0 {
			args = ", args " + argsQual + b.argsName(name, fd.Name)
		}
		var sig string
		switch {
		case b.isRoot(name):
			if b.isSubscriptionRoot(name) {
				ret = "<-chan " + ret
			}
			sig = fmt.Sprintf("(ctx context.Context%s) (%s, error)", args, ret)
		default:
			sig = fmt.Sprintf("(ctx context.Context, obj *%s%s) (%s, error)", b.modelRef(name, name), args, ret)
		}
		out = append(out, resolverSig{
			name:      b.resolverMethod(name, fd.Name),
			signature: sig,
			coord:     name + "." + fd.Name,
			doc:       fd.Description,
		})
	})
	return out
}

// emitGroup renders one group as a single file. Args, the Resolver interface
// and the bindings all live in the same package, so splitting them across
// three files only multiplied the file count: what the compiler rebuilds is
// decided by the package, not the file.
func (b *builder) emitGroup(pkg, group string, withResolver bool) string {
	parts := []string{b.emitArgs(group)}
	if withResolver {
		parts = append(parts, b.emitResolver(group))
	}
	parts = append(parts, b.emitBindings(group, withResolver))

	var body strings.Builder
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		body.WriteString(p)
		if !strings.HasSuffix(p, "\n\n") {
			body.WriteString("\n")
		}
	}
	s := body.String()
	if strings.TrimSpace(s) == "" {
		return ""
	}
	doc := ""
	if pkg != b.pkgName {
		doc = "holds the generated GraphQL bindings for the " + group + " group."
	}
	return b.header(pkg, doc) + b.importBlock(s, "") + s
}
