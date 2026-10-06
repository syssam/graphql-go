package codegen

import (
	"go/types"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// unbindableTypes lists the schema types whose declared Go type the loaded
// packages show cannot be bound, sorted: an enum that cannot back an enum,
// and then anything holding one of those, transitively.
//
// graphql.Enum takes a map from Go value to SDL name, so an enum's Go type has
// to be a comparable type whose values generated code can name. An ORM breaks
// both halves at once: entgql binds an SDL enum to a struct holding a func,
// which is not comparable, and its values are unexported package vars, which
// nothing outside that package can name. One real schema does this 444 times,
// and the binding is written by the ORM into the SDL -- not by hand, so there
// is nothing for the author to correct.
//
// Refusing to generate would block the schema entirely; emitting the binding
// anyway is 12 959 compile errors in files marked DO NOT EDIT. Dropping the
// binding leaves a generated string enum that every field, argument and
// resolver signature agrees on, and the resolver translates it -- which it had
// to do regardless, since only that package can turn the SDL value back into
// its own value. The drop is reported, never silent.
//
// The test is narrow on purpose: only a Go type whose underlying type is not
// basic is refused. A named string type with no constants found is left alone,
// because the constants may simply be somewhere this pass did not load.
func unbindableTypes(schema *ast.Schema, models map[string]string, named map[string]*types.Named) (dropped []string, marshalers map[string]bool) {
	bad := map[string]bool{} // Go types nothing can bind, by "path.Name"
	out := map[string]bool{} // SDL types to drop
	marshalers = map[string]bool{}
	// A type that encodes itself -- MarshalGQL and UnmarshalGQL, the contract
	// gqlgen defines and ent and velox generate -- binds through
	// EnumMarshaler or ScalarMarshaler whatever its underlying type. entgql's
	// OrderField is exactly this: a struct holding a func, dropped until the
	// engine could take it, which sent every orderBy argument and every edge
	// method taking one through a hand-written conversion.
	for _, name := range scalarNames(schema) {
		if n := declaredNamed(models, named, name); n != nil && isGQLMarshaler(n) {
			marshalers[name] = true
		}
	}
	for _, name := range enumNames(schema) {
		n := declaredNamed(models, named, name)
		if n == nil {
			continue // not declared, or not loaded, so nothing is known about it
		}
		if isGQLMarshaler(n) {
			marshalers[name] = true
			continue
		}
		if _, basic := n.Underlying().(*types.Basic); !basic {
			out[name] = true
			bad[goKey(n)] = true
		}
	}
	if len(out) == 0 {
		return nil, marshalers
	}

	// A dropped Go type takes its holders with it. entgql puts the
	// unbindable OrderField inside an Order input that is itself bound to the
	// ORM struct, so dropping only the enum leaves that struct with a field of
	// a type nothing registers -- 452 errors on the real schema, every one an
	// XOrder. A fixpoint rather than one pass, because a holder of a holder is
	// the same problem; the schema's type count bounds the iterations.
	names := declaredNames(schema, models)
	for {
		grew := false
		for _, name := range names {
			if out[name] {
				continue
			}
			n := declaredNamed(models, named, name)
			if n == nil || !referencesAny(n, bad) {
				continue
			}
			out[name] = true
			bad[goKey(n)] = true
			grew = true
		}
		if !grew {
			break
		}
	}
	return sortedKeys(out), marshalers
}

// declaredNames lists every schema type Models binds, sorted, so the fixpoint
// above runs in the same order on every build.
func declaredNames(schema *ast.Schema, models map[string]string) []string {
	var out []string
	for name, def := range schema.Types {
		if def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if _, ok := models[name]; ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// declaredNamed resolves a schema type to the loaded Go type Models declares
// for it, or nil when it is undeclared, predeclared, or not loaded.
func declaredNamed(models map[string]string, named map[string]*types.Named, name string) *types.Named {
	expr, ok := models[name]
	if !ok {
		return nil
	}
	return namedByExpr(named, expr)
}

func goKey(n *types.Named) string {
	if n.Obj().Pkg() == nil {
		return n.Obj().Name()
	}
	return n.Obj().Pkg().Path() + "." + n.Obj().Name()
}

// referencesAny reports whether any field of the struct behind n has one of
// the given Go types, through any number of pointers, slices, arrays or maps.
// Only the type's own fields are read: a field whose own type is bindable is
// not this type's problem, and following it would drop the world.
func referencesAny(n *types.Named, bad map[string]bool) bool {
	st, ok := n.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if elem := namedElem(st.Field(i).Type()); elem != nil && bad[goKey(elem)] {
			return true
		}
	}
	return false
}

// namedElem unwraps a type to the named type it ultimately holds, or nil.
func namedElem(t types.Type) *types.Named {
	for range 8 {
		switch u := types.Unalias(t).(type) {
		case *types.Named:
			return u
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		case *types.Map:
			t = u.Elem()
		default:
			return nil
		}
	}
	return nil
}

// isGQLMarshaler reports whether *n has MarshalGQL(io.Writer) and
// UnmarshalGQL(any) error, the pair graphql.Marshaler requires. MarshalGQL may
// have either receiver, which is why the pointer's method set is the one read.
func isGQLMarshaler(n *types.Named) bool {
	ptr := types.NewPointer(n)
	sig := func(name string) *types.Signature {
		obj, _, _ := types.LookupFieldOrMethod(ptr, true, n.Obj().Pkg(), name)
		fn, ok := obj.(*types.Func)
		if !ok {
			return nil
		}
		return fn.Type().(*types.Signature)
	}
	m, u := sig("MarshalGQL"), sig("UnmarshalGQL")
	if m == nil || u == nil {
		return false
	}
	if m.Params().Len() != 1 || m.Results().Len() != 0 || types.TypeString(m.Params().At(0).Type(), nil) != "io.Writer" {
		return false
	}
	if u.Params().Len() != 1 || u.Results().Len() != 1 || types.TypeString(u.Results().At(0).Type(), nil) != "error" {
		return false
	}
	_, isIface := u.Params().At(0).Type().Underlying().(*types.Interface)
	return isIface
}
