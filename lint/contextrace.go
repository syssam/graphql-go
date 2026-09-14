// Package lint holds static checks for invariants in graphql-go that the
// compiler cannot express and tests catch only by luck.
//
// It is a separate module so the library's dependency policy — gqlparser and
// the standard library, nothing else — is unaffected.
package lint

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// ContextType is the fully qualified type whose Get/Set pair is unsafe to
// combine. It is a variable so the analyzer's own tests can point it at a
// fixture rather than depending on graphql-go.
var ContextType = "github.com/syssam/graphql-go.OperationContext"

// ContextRace reports OperationContext.Get followed by Set on the same key.
//
// Request-scoped extensions install their per-operation state that way, and
// the pair is a check-then-act race: concurrent sibling resolvers reach their
// first Load together, every one of them misses the Get, and every one installs
// its own state. Only the last write survives while the rest keep using
// orphaned copies.
//
// The failure is silent — batching quietly degrades to N+1 and the per-request
// cache splits — and invisible to the race detector, because Get and Set are
// each individually locked. It was found in this codebase only because a test
// happened to fail 4 times in 40 runs. GetOrSet does the pair atomically.
var ContextRace = &analysis.Analyzer{
	Name:     "contextrace",
	Doc:      "report OperationContext.Get followed by Set on the same key; use GetOrSet",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      runContextRace,
}

// call records one Get or Set on an OperationContext.
type call struct {
	recv string // receiver expression, rendered
	key  string // key expression, rendered
	pos  ast.Node
}

func runContextRace(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// Bodies are examined one at a time: a Get in one function and a Set in
	// another are not the pattern this looks for.
	bodies := []ast.Node{(*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)}
	insp.Preorder(bodies, func(n ast.Node) {
		var body *ast.BlockStmt
		switch fn := n.(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		}
		if body == nil {
			return
		}

		var gets, sets []call
		ast.Inspect(body, func(n ast.Node) bool {
			// Nested function literals are visited as their own body.
			if _, ok := n.(*ast.FuncLit); ok && n != body {
				return true
			}
			c, method, ok := operationContextCall(pass, n)
			if !ok {
				return true
			}
			switch method {
			case "Get":
				gets = append(gets, c)
			case "Set":
				sets = append(sets, c)
			}
			return true
		})

		for _, g := range gets {
			for _, s := range sets {
				if g.recv != s.recv || g.key != s.key {
					continue
				}
				// Only a Get that precedes the Set is check-then-act. Setting
				// first and reading back later is ordinary use.
				if g.pos.Pos() >= s.pos.Pos() {
					continue
				}
				pass.Reportf(s.pos.Pos(),
					"%s.Get(%s) followed by Set on the same key is a check-then-act race; use GetOrSet",
					g.recv, g.key)
			}
		}
	})
	return nil, nil
}

// operationContextCall reports whether n is a Get or Set call on an
// *OperationContext, and renders its receiver and key.
func operationContextCall(pass *analysis.Pass, n ast.Node) (call, string, bool) {
	ce, ok := n.(*ast.CallExpr)
	if !ok {
		return call{}, "", false
	}
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return call{}, "", false
	}
	method := sel.Sel.Name
	if method != "Get" && method != "Set" {
		return call{}, "", false
	}
	if len(ce.Args) == 0 {
		return call{}, "", false
	}
	if !isOperationContext(pass.TypesInfo.TypeOf(sel.X)) {
		return call{}, "", false
	}
	return call{
		recv: render(sel.X),
		key:  render(ce.Args[0]),
		pos:  ce,
	}, method, true
}

func isOperationContext(t types.Type) bool {
	if t == nil {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path()+"."+named.Obj().Name() == ContextType
}

// render prints an expression compactly, so two occurrences of the same
// receiver or key compare equal.
func render(e ast.Expr) string {
	var b strings.Builder
	writeExpr(&b, e)
	return b.String()
}

func writeExpr(b *strings.Builder, e ast.Expr) {
	switch x := e.(type) {
	case *ast.Ident:
		b.WriteString(x.Name)
	case *ast.SelectorExpr:
		writeExpr(b, x.X)
		b.WriteByte('.')
		b.WriteString(x.Sel.Name)
	case *ast.StarExpr:
		b.WriteByte('*')
		writeExpr(b, x.X)
	case *ast.BasicLit:
		b.WriteString(x.Value)
	case *ast.IndexExpr:
		writeExpr(b, x.X)
		b.WriteByte('[')
		writeExpr(b, x.Index)
		b.WriteByte(']')
	case *ast.CallExpr:
		writeExpr(b, x.Fun)
		b.WriteString("(...)")
	default:
		b.WriteString("?")
	}
}
