package codegen

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// bindingChanges names each field whose answer moves between a Resolver
// method, a struct field and a method of the bound type, as
// "Type.field: resolver TaskCreatedAt → field CreatedAt", sorted.
//
// AutoBind decides a field from the Go type, so an ORM gaining a CreatedAt
// rebinds Task.createdAt from the resolver that derived it to the stored
// column: a different value, served without a word, since the only trace was
// a rewritten generated.go and a Resolver method left over. The old answer
// is read back from the generated.go on disk and the new one from this run's;
// both are gqlc's own output, so what is read is what the emitters write.
//
// Only a generated.go both runs have is compared. A field whose group was
// renamed or merged arrives in a file the last run did not write and is not
// reported; that change already rewrites every file in the group.
func (b *builder) bindingChanges(rels []string, olds [][]byte, files map[string][]byte) []string {
	roots := map[string]string{}
	for call, def := range map[string]*ast.Definition{"Query": b.schema.Query, "Mutation": b.schema.Mutation, "Subscription": b.schema.Subscription} {
		if def != nil {
			roots[call] = def.Name
		}
	}
	before, after := map[string]string{}, map[string]string{}
	for i, rel := range rels {
		if olds[i] == nil || (rel != "generated.go" && !strings.HasSuffix(rel, "/generated.go")) || sameContent(olds[i], files[rel]) {
			continue
		}
		maps.Copy(before, bindingSources(olds[i], roots))
		maps.Copy(after, bindingSources(files[rel], roots))
	}
	var out []string
	for _, coord := range slices.Sorted(maps.Keys(after)) {
		if was, ok := before[coord]; ok && was != after[coord] {
			out = append(out, coord+": "+was+" → "+after[coord])
		}
	}
	return out
}

// bindingSources reads what answers each field in one generated.go, by
// coordinate: "resolver M" for a call to the Resolver, "field F" and
// "method M" for the bound type's own. roots maps the root binding calls
// (graphql.Query, ...) to the schema's names for those types.
//
// It reads the shapes fieldCall and manifestFieldCall write: the last
// function literal among a binding's arguments, returning r.M(...), v.M(...)
// or v.F, possibly through a conversion. Anything else is left out, which
// makes a change to or from it unreported rather than wrongly reported.
func bindingSources(src []byte, roots map[string]string) map[string]string {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	goast.Inspect(f, func(n goast.Node) bool {
		call, ok := n.(*goast.CallExpr)
		if !ok {
			return true
		}
		var typeName string
		args := call.Args
		switch fun := call.Fun.(type) {
		case *goast.IndexExpr: // graphql.Object[T]("Name", fields...)
			if engineCall(fun.X) != "Object" || len(args) == 0 {
				return true
			}
			typeName, ok = stringLit(args[0])
			if !ok {
				return true
			}
			args = args[1:]
		default:
			if typeName = roots[engineCall(fun)]; typeName == "" {
				return true
			}
		}
		for _, a := range args {
			field, ok := a.(*goast.CallExpr)
			if !ok || len(field.Args) < 2 {
				continue
			}
			name, ok := stringLit(field.Args[0])
			if !ok {
				continue
			}
			if src := fieldSource(field.Args[1:]); src != "" {
				out[typeName+"."+name] = src
			}
		}
		return false
	})
	return out
}

// engineCall is the name in graphql.Name, or "".
func engineCall(e goast.Expr) string {
	sel, ok := e.(*goast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, ok := sel.X.(*goast.Ident); !ok || pkg.Name != "graphql" {
		return ""
	}
	return sel.Sel.Name
}

func stringLit(e goast.Expr) (string, bool) {
	lit, ok := e.(*goast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// fieldSource describes what the function literal among args returns.
func fieldSource(args []goast.Expr) string {
	for _, a := range args {
		fn, ok := a.(*goast.FuncLit)
		if !ok || fn.Body == nil || len(fn.Body.List) != 1 {
			continue
		}
		ret, ok := fn.Body.List[0].(*goast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return ""
		}
		return describeAnswer(ret.Results[0])
	}
	return ""
}

func describeAnswer(e goast.Expr) string {
	switch e := e.(type) {
	case *goast.SelectorExpr:
		if recv(e.X) == "v" {
			return "field " + e.Sel.Name
		}
	case *goast.CallExpr:
		if sel, ok := e.Fun.(*goast.SelectorExpr); ok {
			switch recv(sel.X) {
			case "r":
				return "resolver " + sel.Sel.Name
			case "v":
				return "method " + sel.Sel.Name
			}
		}
		// A conversion, graphql.ID(v.ID).
		if len(e.Args) == 1 {
			return describeAnswer(e.Args[0])
		}
	}
	return ""
}

func recv(e goast.Expr) string {
	if id, ok := e.(*goast.Ident); ok {
		return id.Name
	}
	return ""
}

// reportBindingChanges is the note a generate makes of bindingChanges.
func (b *builder) reportBindingChanges(changes []string) {
	if len(changes) == 0 {
		return
	}
	b.notef("%d field(s) are now answered by something else, so the value served may differ; check each is meant:\n  %s",
		len(changes), strings.Join(changes, "\n  "))
}
