// Package lint holds static checks for invariants in graphql-go that the
// compiler cannot express and tests catch only by luck.
//
// It is a separate module so the library's dependency policy — gqlparser and
// the standard library, nothing else — is unaffected.
package lint

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
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
//
// What it sees is one function at a time, closures included. Within that it
// follows the context through an alias, a struct that embeds it, a method
// value or expression, and an interface variable it was assigned to; it
// compares keys by value, so a constant, a literal and a variable holding
// either are one key; and it places a call made inside a closure bound to a
// variable where the variable is used, not where the closure is written.
//
// What it does not see: a Get in one function and the Set in another. A
// helper that looks the state up and a second that installs it are the same
// race and pass clean, as does a context reached through an interface
// parameter. A clean run means no pair was written in one place, not that
// none exists.
var ContextRace = &analysis.Analyzer{
	Name: "contextrace",
	Doc:  "report OperationContext.Get followed by Set on the same key within one function; use GetOrSet",
	Run:  runContextRace,
}

// call records one Get or Set on an OperationContext.
type call struct {
	text string       // the call as written, for the message
	key  string       // what the key is, so two spellings of one key compare equal
	recv types.Object // the parameter the context came in as, when that can be told
	sig  *ast.FuncType
	pos  token.Pos   // where the call is written
	at   []token.Pos // where it runs
}

func runContextRace(pass *analysis.Pass) (any, error) {
	f := newFacts(pass)

	// A body is walked with the closures inside it, because a Get in a
	// function and a Set in a closure it makes -- under a sync.Once, on a
	// goroutine -- are one check-then-act. A closure is also a body of its
	// own, so a pair wholly inside one is found twice, once from each; each
	// Set is reported once.
	reported := map[token.Pos]bool{}
	for _, body := range f.bodies {
		var gets, sets []call
		ast.Inspect(body, func(n ast.Node) bool {
			c, method, ok := f.contextCall(n)
			if !ok {
				return true
			}
			c.at = f.runsAt(c.pos, body, 0)
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
				if g.key != s.key {
					continue
				}
				// One operation has one context, and an alias, a field holding
				// it or a second OperationFrom all reach the same state, so the
				// receiver's spelling says nothing. Two parameters of one
				// function do: a signature that takes two contexts is moving
				// state between operations, not installing it in one.
				if g.recv != nil && s.recv != nil && g.recv != s.recv && g.sig == s.sig {
					continue
				}
				// Only a Get that can run before the Set is check-then-act.
				// Setting first and reading back later is ordinary use.
				if len(g.at) == 0 || len(s.at) == 0 || slices.Min(g.at) >= slices.Max(s.at) {
					continue
				}
				if reported[s.pos] {
					continue
				}
				reported[s.pos] = true
				pass.Reportf(s.pos,
					"%s followed by Set on the same key is a check-then-act race; use GetOrSet", g.text)
			}
		}
	}
	return nil, nil
}

// facts is what one pass knows about its package beyond the types: what each
// variable was given, which closures have a name, and where names are used.
type facts struct {
	info *types.Info

	bodies []*ast.BlockStmt
	lits   []*ast.FuncLit

	// inits is the expression a variable was declared with. A variable that
	// is written anywhere else is in written, and then inits says nothing
	// about what it holds.
	inits   map[types.Object]ast.Expr
	written map[types.Object]bool
	// params maps a parameter or receiver to the signature declaring it.
	params map[types.Object]*ast.FuncType
	// bound is the variable a function literal was declared as.
	bound map[*ast.FuncLit]types.Object
	uses  map[types.Object][]token.Pos
}

func newFacts(pass *analysis.Pass) *facts {
	f := &facts{
		info:    pass.TypesInfo,
		inits:   map[types.Object]ast.Expr{},
		written: map[types.Object]bool{},
		params:  map[types.Object]*ast.FuncType{},
		bound:   map[*ast.FuncLit]types.Object{},
		uses:    map[types.Object][]token.Pos{},
	}
	for id, obj := range f.info.Uses {
		if _, ok := obj.(*types.Var); ok {
			f.uses[obj] = append(f.uses[obj], id.Pos())
		}
	}
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Body != nil {
					f.bodies = append(f.bodies, n.Body)
				}
				f.declareParams(n.Recv, n.Type)
				f.declareParams(n.Type.Params, n.Type)
			case *ast.FuncLit:
				f.bodies = append(f.bodies, n.Body)
				f.lits = append(f.lits, n)
				f.declareParams(n.Type.Params, n.Type)
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					id, ok := ast.Unparen(lhs).(*ast.Ident)
					if !ok {
						f.markWritten(lhs)
						continue
					}
					if obj := f.info.Defs[id]; obj != nil && len(n.Lhs) == len(n.Rhs) {
						f.inits[obj] = n.Rhs[i]
					} else if obj := f.info.Uses[id]; obj != nil {
						f.written[obj] = true
					}
				}
			case *ast.ValueSpec:
				for i, id := range n.Names {
					if obj := f.info.Defs[id]; obj != nil && len(n.Names) == len(n.Values) {
						f.inits[obj] = n.Values[i]
					}
				}
			case *ast.IncDecStmt:
				f.markWritten(n.X)
			case *ast.RangeStmt:
				if n.Tok == token.ASSIGN {
					f.markWritten(n.Key)
					f.markWritten(n.Value)
				}
			case *ast.UnaryExpr:
				// Through a pointer anything may be stored.
				if n.Op == token.AND {
					f.markWritten(n.X)
				}
			}
			return true
		})
	}
	for obj, init := range f.inits {
		if lit, ok := ast.Unparen(init).(*ast.FuncLit); ok && !f.written[obj] {
			f.bound[lit] = obj
		}
	}
	return f
}

func (f *facts) declareParams(list *ast.FieldList, sig *ast.FuncType) {
	if list == nil {
		return
	}
	for _, field := range list.List {
		for _, id := range field.Names {
			if obj := f.info.Defs[id]; obj != nil {
				f.params[obj] = sig
			}
		}
	}
}

// markWritten records a write to the variable e names or is part of: k.name
// = x changes what k holds as surely as k = x does.
func (f *facts) markWritten(e ast.Expr) {
	for e != nil {
		switch x := ast.Unparen(e).(type) {
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.Ident:
			if obj := f.info.Uses[x]; obj != nil {
				f.written[obj] = true
			}
			return
		default:
			return
		}
	}
}

// init is the expression a variable still holds: the one it was declared
// with, when nothing has written to it since.
func (f *facts) init(obj types.Object) ast.Expr {
	if obj == nil || f.written[obj] {
		return nil
	}
	return f.inits[obj]
}

// variable is the variable an identifier or a qualified one names.
func (f *facts) variable(e ast.Expr) *types.Var {
	var id *ast.Ident
	switch e := e.(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		if _, isSelection := f.info.Selections[e]; isSelection {
			return nil
		}
		id = e.Sel
	default:
		return nil
	}
	v, _ := f.info.Uses[id].(*types.Var)
	return v
}

// The chains followed below -- a variable declared from a variable, a closure
// used inside a closure -- are short in real code. The bound is there so that
// a cycle the type checker allows cannot hang the analysis.
const maxFollow = 8

// contextCall reports whether n is a Get or Set call on an OperationContext.
func (f *facts) contextCall(n ast.Node) (call, string, bool) {
	ce, ok := n.(*ast.CallExpr)
	if !ok {
		return call{}, "", false
	}
	method, recv, keyArg, ok := f.contextMethod(ce.Fun, 0)
	if !ok || len(ce.Args) <= keyArg {
		return call{}, "", false
	}
	if keyArg == 1 {
		// A method expression takes its receiver as the first argument.
		recv = ce.Args[0]
	}
	c := call{
		text: types.ExprString(ce),
		key:  f.keyOf(ce.Args[keyArg], 0),
		pos:  ce.Pos(),
	}
	c.recv, c.sig = f.param(recv, 0)
	return c, method, true
}

// contextMethod reports whether fun, the function of a call, is
// OperationContext's Get or Set, which argument is the key, and the expression
// the context is taken from.
func (f *facts) contextMethod(fun ast.Expr, depth int) (method string, recv ast.Expr, keyArg int, ok bool) {
	if depth > maxFollow {
		return "", nil, 0, false
	}
	fun = ast.Unparen(fun)
	sel, isSel := fun.(*ast.SelectorExpr)
	selection := f.info.Selections[sel]
	if !isSel || selection == nil {
		// get := oc.Get, and then get(key).
		if v := f.variable(fun); v != nil {
			if init := f.init(v); init != nil {
				return f.contextMethod(init, depth+1)
			}
		}
		return "", nil, 0, false
	}

	fn, isFunc := selection.Obj().(*types.Func)
	if !isFunc || (fn.Name() != "Get" && fn.Name() != "Set") {
		return "", nil, 0, false
	}
	owner := fn.Signature().Recv()
	if owner == nil {
		return "", nil, 0, false
	}
	switch {
	// The method's own receiver, not the selected expression's type: that is
	// the same method when it is reached through an embedding struct.
	case isOperationContext(owner.Type()):
		if selection.Kind() == types.MethodExpr {
			return fn.Name(), nil, 1, true
		}
		return fn.Name(), sel.X, 0, true
	case types.IsInterface(owner.Type()) && selection.Kind() == types.MethodVal && f.holdsContext(sel.X, 0):
		return fn.Name(), sel.X, 0, true
	}
	return "", nil, 0, false
}

// holdsContext reports whether e, an expression of interface type, was given
// an OperationContext where this package can see it.
func (f *facts) holdsContext(e ast.Expr, depth int) bool {
	if depth > maxFollow {
		return false
	}
	e = ast.Unparen(e)
	if isOperationContext(f.info.TypeOf(e)) {
		return true
	}
	if conv, ok := e.(*ast.CallExpr); ok && len(conv.Args) == 1 {
		if tv, ok := f.info.Types[conv.Fun]; ok && tv.IsType() {
			return f.holdsContext(conv.Args[0], depth+1)
		}
		return false
	}
	if v := f.variable(e); v != nil {
		if init := f.init(v); init != nil {
			return f.holdsContext(init, depth+1)
		}
	}
	return false
}

// param is the parameter e was passed in as, followed back through the
// variables it was copied to, and the signature that declares it.
func (f *facts) param(e ast.Expr, depth int) (types.Object, *ast.FuncType) {
	if e == nil || depth > maxFollow {
		return nil, nil
	}
	e = ast.Unparen(e)
	if conv, ok := e.(*ast.CallExpr); ok && len(conv.Args) == 1 {
		if tv, ok := f.info.Types[conv.Fun]; ok && tv.IsType() {
			return f.param(conv.Args[0], depth+1)
		}
		return nil, nil
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return nil, nil
	}
	obj := f.info.Uses[id]
	if sig, ok := f.params[obj]; ok {
		if f.written[obj] {
			return nil, nil
		}
		return obj, sig
	}
	if init := f.init(obj); init != nil {
		return f.param(init, depth+1)
	}
	return nil, nil
}

// keyOf renders a key so that two expressions with one value compare equal
// and two with different values do not. Get and Set compare keys as interface
// values, so that is type and value: a constant is its type and value however
// it is spelled, a struct literal its type and fields, and a variable is what
// it was declared with when that is a value and not a fresh pointer.
func (f *facts) keyOf(e ast.Expr, depth int) string {
	e = ast.Unparen(e)
	if depth > maxFollow {
		return "expr " + types.ExprString(e)
	}
	if tv, ok := f.info.Types[e]; ok && tv.Value != nil {
		return "const " + typeName(types.Default(tv.Type)) + " " + tv.Value.ExactString()
	}
	switch e := e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		v := f.variable(e)
		if v == nil || v.IsField() {
			break
		}
		if init := f.init(v); init != nil && f.isValue(init) {
			return f.keyOf(init, depth+1)
		}
		pkg := ""
		if v.Pkg() != nil {
			pkg = v.Pkg().Path()
		}
		return fmt.Sprintf("var %s.%s@%d", pkg, v.Name(), v.Pos())
	case *ast.CompositeLit:
		if !f.isValue(e) {
			break
		}
		var s strings.Builder
		s.WriteString("value " + typeName(f.info.TypeOf(e)) + "{")
		for _, elt := range e.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				s.WriteString(types.ExprString(kv.Key) + ":")
				elt = kv.Value
			}
			s.WriteString(f.keyOf(elt, depth+1) + ",")
		}
		return s.String() + "}"
	}
	return "expr " + types.ExprString(e)
}

// isValue reports whether evaluating e twice gives equal keys. A struct or
// array literal does; &T{}, new(T) and a call give a different pointer, or an
// unknown value, each time.
func (f *facts) isValue(e ast.Expr) bool {
	e = ast.Unparen(e)
	if tv, ok := f.info.Types[e]; ok && tv.Value != nil {
		return true
	}
	switch e := e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return f.variable(e) != nil
	case *ast.CompositeLit:
		t := f.info.TypeOf(e)
		if t == nil {
			return false
		}
		switch t.Underlying().(type) {
		case *types.Struct, *types.Array:
			return true
		}
	}
	return false
}

func typeName(t types.Type) string {
	if t == nil {
		return "?"
	}
	return types.TypeString(types.Unalias(t), nil)
}

// runsAt is where a call written at pos runs, as positions in body. Written
// directly in body, or in a closure with no name, that is where it stands.
// Inside a closure bound to a variable it is wherever the variable is used:
// an installer declared at the top of a function and called under the check
// runs under the check.
func (f *facts) runsAt(pos token.Pos, body *ast.BlockStmt, depth int) []token.Pos {
	var outer *ast.FuncLit
	for _, lit := range f.lits {
		if lit.Body == body || lit.Pos() < body.Pos() || lit.End() > body.End() {
			continue
		}
		if lit.Pos() <= pos && pos < lit.End() && (outer == nil || lit.Pos() < outer.Pos()) {
			outer = lit
		}
	}
	if outer == nil {
		return []token.Pos{pos}
	}
	name := f.bound[outer]
	if name == nil || depth > maxFollow {
		return []token.Pos{pos}
	}
	var at []token.Pos
	for _, use := range f.uses[name] {
		// A use inside the closure itself is recursion, which runs only once
		// some other use has.
		if use < body.Pos() || use >= body.End() || (outer.Pos() <= use && use < outer.End()) {
			continue
		}
		at = append(at, f.runsAt(use, body, depth+1)...)
	}
	return at
}

func isOperationContext(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path()+"."+named.Obj().Name() == ContextType
}
