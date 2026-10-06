package codegen

import (
	"go/types"
	"maps"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"golang.org/x/tools/go/packages"
)

// altEnumTypes finds a second Go type for an SDL enum.
//
// The registry is keyed on (GraphQL type, reflect.Type), so one SDL enum can
// have two Go types, and an ORM produces exactly that: @goModel binds
// AssetDepreciationMethod to velox/asset.DepreciationMethod, while the filter
// struct the same ORM generates carries velox/assetdepreciation.Method for the
// same column. Nothing here is a mistake -- both types exist, both carry the
// same values -- but only one of them is bound, and every input field using
// the other is a NewSchema error the author cannot see from the SDL.
//
// Only input objects are searched. An object field of the unbound type falls
// to a resolver, which the author writes and can convert in; an input field
// has nowhere to put that conversion.
func altEnumTypes(schema *ast.Schema, models map[string]string, found map[string]*types.Named, idx map[string]*types.Named) map[string]*types.Named {
	primary := boundEnumTypes(schema, models, found)
	out := map[string]*types.Named{}
	// Input names and fields are walked in sorted and SDL order, so a third Go
	// type for one enum loses to the second every run rather than by map
	// iteration.
	for _, name := range inputNames(schema) {
		named := found[name]
		if expr, declared := models[name]; declared {
			named = namedByExpr(idx, expr)
		}
		if named == nil {
			continue
		}
		fields := structFields(named)
		for _, fd := range schema.Types[name].Fields {
			enum := fd.Type.Name()
			if _, taken := out[enum]; taken {
				continue
			}
			if alt := altTypeAt(schema, fields, fd, primary[enum]); alt != nil {
				out[enum] = alt
			}
		}
	}
	return out
}

// boundEnumTypes maps each SDL enum to the "importPath.TypeName" of the single
// Go type it is bound to, declared or discovered.
func boundEnumTypes(schema *ast.Schema, models map[string]string, found map[string]*types.Named) map[string]string {
	out := map[string]string{}
	for _, name := range enumNames(schema) {
		if expr, ok := models[name]; ok {
			if path, _ := splitModelExpr(expr); path != "" {
				out[name] = path + expr[strings.LastIndex(expr, "."):]
			}
			continue
		}
		if n, ok := found[name]; ok && n.Obj().Pkg() != nil {
			out[name] = n.Obj().Pkg().Path() + "." + n.Obj().Name()
		}
	}
	return out
}

// altTypeAt yields the Go type backing one input field when the field is an
// enum whose bound type is not the one the struct holds. bound is empty for an
// enum nothing binds, which is a different problem and not this one's to fix.
func altTypeAt(schema *ast.Schema, fields map[string]goField, fd *ast.FieldDefinition, bound string) *types.Named {
	if bound == "" {
		return nil
	}
	if def := schema.Types[fd.Type.Name()]; def == nil || def.Kind != ast.Enum {
		return nil
	}
	f, ok := matchField(fields, fd.Name)
	if !ok {
		return nil
	}
	n := namedLeaf(f.typ)
	if n == nil || n.Obj().Pkg() == nil {
		return nil
	}
	if n.Obj().Pkg().Path()+"."+n.Obj().Name() == bound {
		return nil
	}
	return n
}

// namedLeaf unwraps pointers, slices and arrays down to the named type a leaf
// binding would need, and yields nothing for anything else.
func namedLeaf(t types.Type) *types.Named {
	for range 4 {
		switch u := types.Unalias(t).(type) {
		case *types.Named:
			if _, ok := u.Underlying().(*types.Basic); ok {
				return u
			}
			return nil
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		default:
			return nil
		}
	}
	return nil
}

// altEnumPkgs loads the packages holding the alternate types, which no
// AutoBind pattern and no @goModel named -- they are reachable only through a
// bound struct's field, and export data for a dependency does not come back
// from packages.Load as a package of its own.
func altEnumPkgs(dir string, alt map[string]*types.Named, loaded []*packages.Package, loads *int) []*packages.Package {
	have := map[string]bool{}
	for _, p := range loaded {
		have[p.PkgPath] = true
	}
	var want []string
	for _, name := range slices.Sorted(maps.Keys(alt)) {
		path := alt[name].Obj().Pkg().Path()
		if have[path] {
			continue
		}
		have[path] = true
		want = append(want, path)
	}
	if len(want) == 0 {
		return nil
	}
	*loads++
	pkgs, err := packages.Load(&packages.Config{Dir: dir, Mode: loadMode}, want...)
	if err != nil {
		return nil
	}
	return pkgs
}

// altEnumBindings turns the alternate types into bindings, and only when every
// SDL value of the enum is accounted for. A partial map would bind some values
// and leave the rest unrepresentable at that position, which is worse than the
// error it replaces: the first is silent at run time, the second stops the
// build. What it cannot bind it reports, because the author can write the map
// by hand and cannot be expected to guess that they need to.
func altEnumBindings(schema *ast.Schema, alt map[string]*types.Named, consts map[string]*enumConsts, notef func(string, ...any)) []TypeBinding {
	var out []TypeBinding
	var unbound []string
	for _, name := range slices.Sorted(maps.Keys(alt)) {
		named := alt[name]
		key := named.Obj().Pkg().Path() + "." + named.Obj().Name()
		def := schema.Types[name]
		values := map[string]string{}
		if vals := consts[key]; vals != nil {
			for _, ev := range def.EnumValues {
				if c, ok := vals.lookup(ev.Name); ok {
					values[ev.Name] = c
				}
			}
		}
		if len(values) != len(def.EnumValues) {
			unbound = append(unbound, name+" as "+key)
			continue
		}
		out = append(out, TypeBinding{
			Name:   name,
			Go:     GoType{PkgPath: named.Obj().Pkg().Path(), Name: named.Obj().Name()},
			Values: values,
		})
	}
	if len(unbound) > 0 {
		notef("%d enums are used at a second Go type whose constants could not all be found, so every input field using that type will fail at NewSchema; bind each with graphql.Enum (%s)",
			len(unbound), summarize(unbound, 8))
	}
	return out
}
