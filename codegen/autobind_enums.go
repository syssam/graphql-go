package codegen

import (
	"go/constant"
	"go/types"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"golang.org/x/tools/go/packages"
)

// constIndex maps "importPath.TypeName" to that type's string constants, keyed
// by the string each one carries rather than by its identifier.
//
// The value is the key because the value is what the SDL says. A Go constant
// named AccessPolicyExpectVisible carrying "VISIBLE" answers the SDL value
// VISIBLE exactly; nothing about its identifier says so.
func constIndex(pkgs []*packages.Package) map[string]*enumConsts {
	out := map[string]*enumConsts{}
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		// scope.Names is sorted, so which constant wins a collision is the
		// same on every run.
		for _, name := range scope.Names() {
			c, ok := scope.Lookup(name).(*types.Const)
			if !ok || !c.Exported() || c.Val().Kind() != constant.String {
				continue
			}
			named, ok := types.Unalias(c.Type()).(*types.Named)
			if !ok || named.Obj().Pkg() == nil {
				continue
			}
			v := constant.StringVal(c.Val())
			if v == "" {
				continue
			}
			key := named.Obj().Pkg().Path() + "." + named.Obj().Name()
			ec, seen := out[key]
			if !seen {
				ec = &enumConsts{exact: map[string]string{}, folded: map[string]string{}}
				out[key] = ec
			}
			ec.add(v, c.Name())
		}
	}
	return out
}

// enumConsts holds one Go type's string constants by the value each carries,
// exactly and case-folded.
//
// Case folding is not a convenience. ent stores an enum column lower-case and
// upper-cases it on the way out (MarshalGQL is strconv.Quote(ToUpper(...))),
// so EventTypeView = "view" is the constant for the SDL value VIEW, and an
// exact match finds nothing for an entire ORM's worth of enums. A folded key
// two constants share is ambiguous and yields neither: guessing there would
// bind the wrong one silently, where binding nothing is the compile error the
// author already had.
type enumConsts struct {
	exact  map[string]string
	folded map[string]string
}

func (e *enumConsts) add(value, name string) {
	if _, taken := e.exact[value]; !taken {
		e.exact[value] = name
	}
	up := strings.ToUpper(value)
	if prev, taken := e.folded[up]; taken {
		if prev != name {
			e.folded[up] = "" // ambiguous
		}
		return
	}
	e.folded[up] = name
}

func (e *enumConsts) lookup(sdlValue string) (string, bool) {
	if e == nil {
		return "", false
	}
	if c, ok := e.exact[sdlValue]; ok {
		return c, true
	}
	c, ok := e.folded[strings.ToUpper(sdlValue)]
	return c, ok && c != ""
}

// enumBindings discovers the Go constants behind an enum's SDL values.
//
// The generator derives a constant name from the SDL value for an enum whose
// model it writes itself, which is sound because it wrote both sides. For an
// enum bound to a hand-written or ORM-generated Go type the same derivation is
// a guess, and every wrong guess is a compile error in a file marked DO NOT
// EDIT -- one real schema spells VISIBLE as AccessPolicyExpectVisible where
// the derivation produces AccessPolicyExpectationVisible, and its ORM spells
// WORKSPACE_ID as WorkspaceID where the derivation produces Workspace_id.
func enumBindings(schema *ast.Schema, models map[string]string, found map[string]*types.Named, consts map[string]*enumConsts) []TypeBinding {
	var out []TypeBinding
	for _, name := range enumNames(schema) {
		def := schema.Types[name]
		var key string
		declared := false
		if expr, ok := models[name]; ok {
			path, _ := splitModelExpr(expr)
			if path == "" {
				continue // a predeclared type carries no named constants
			}
			key, declared = path+expr[strings.LastIndex(expr, "."):], true
		} else if named, ok := found[name]; ok && named.Obj().Pkg() != nil {
			key = named.Obj().Pkg().Path() + "." + named.Obj().Name()
		} else {
			continue
		}
		vals := consts[key]
		if vals == nil {
			continue
		}
		values := map[string]string{}
		for _, ev := range def.EnumValues {
			if c, ok := vals.lookup(ev.Name); ok {
				values[ev.Name] = c
			}
		}
		if len(values) == 0 {
			continue
		}
		tb := TypeBinding{Name: name, Values: values}
		if !declared {
			// An undiscovered value would fall back to a name derived in
			// the generated model package, so a partial discovery has to
			// bind nothing: the generator owns both sides or neither.
			if len(values) != len(def.EnumValues) {
				continue
			}
			named := found[name]
			tb.Go = GoType{PkgPath: named.Obj().Pkg().Path(), Name: named.Obj().Name()}
		}
		// A declared enum missing a value keeps the derived name for it,
		// which will not compile -- but it did not compile before either,
		// and refusing to generate would stop a schema that works today.
		out = append(out, tb)
	}
	return out
}
