package codegen

import (
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// discoverFields applies the binding order to every field of a type: struct
// field, then method, then resolver. Anything not confidently matched is left
// to the resolver, which can always be written; a wrong guess would instead be
// a compile error in code the user did not write.
//
// Only the type's own fields are searched, and embedded ones because Go
// promotes them. **Do not extend this to named nested structs.** ent and the
// ORMs derived from it keep relations in an Edges field, and reaching into it
// looks like an obvious win -- AbTesting.variants is AbTesting.Edges.Variants,
// and on a real 5 503-type schema that pattern covers thousands of fields.
// It was built, measured, and reverted:
//
//	func (m *AbTesting) ParentTest(ctx) (*AbTesting, error) {
//	    result, err := m.Edges.ParentTestOrErr()
//	    if runtime.IsNotLoaded(err) { ... m.QueryParentTest().Only(ctx) ... }
//	}
//
// The method is a lazy loader. Edges.ParentTest is the raw field, nil whenever
// the edge was not eager-loaded, so binding to it returns null for data that
// exists -- silently, with no error anywhere. The resolver count does not even
// improve: AutoBind already matches these as methods, which is the correct
// binding. Reaching through the container would only demote a correct method
// call to a wrong field read.
func discoverFields(def *ast.Definition, named *types.Named, want func(*ast.Type, string, bool) string, qual types.Qualifier) map[string]FieldBinding {
	fields := structFields(named)
	methods := methodSet(named)

	out := map[string]FieldBinding{}
	for _, fd := range def.Fields {
		if strings.HasPrefix(fd.Name, "__") {
			continue
		}
		expected := want(fd.Type, "", false)
		if len(fd.Arguments) == 0 {
			if f, ok := matchField(fields, fd.Name); ok {
				if conv, usable := reconcile(f.typ, expected, qual); usable {
					out[fd.Name] = FieldBinding{Kind: FieldStruct, GoName: f.name, Convert: conv}
					continue
				}
				if conv, usable := reconcileValue(f.typ, expected, qual); usable {
					out[fd.Name] = FieldBinding{Kind: FieldStruct, GoName: f.name, Convert: conv, Value: true}
					continue
				}
			}
		}
		if m, ok := matchMethod(methods, fd.Name); ok && m.fits(fd.Arguments, want, qual) {
			if conv, usable := reconcile(m.result, expected, qual); usable {
				out[fd.Name] = FieldBinding{
					Kind:    FieldMethod,
					GoName:  m.name,
					Context: m.context,
					Error:   m.errors,
					Convert: conv,
				}
				continue
			}
		}
		out[fd.Name] = FieldBinding{Kind: FieldResolver}
	}
	return out
}

// goField is a struct field found at some embedding depth.
type goField struct {
	name string
	tag  string
	typ  types.Type
	deep int
	// ambiguous marks a name two embedded structs promote from one depth.
	ambiguous bool
	// hidden marks a field tagged json:"-".
	hidden bool
}

// structFields collects the fields of a struct, following embedded ones. A
// shallower field wins, which is the promotion rule Go itself uses, and so is
// the other half of it: a name promoted twice from one depth is ambiguous and
// selects nothing, so it is left out rather than bound to the first one met.
func structFields(named *types.Named) map[string]goField {
	out := map[string]goField{}
	var walk func(t types.Type, depth int)
	seen := map[types.Type]bool{}
	walk = func(t types.Type, depth int) {
		if depth > 8 || seen[t] {
			return
		}
		seen[t] = true
		st, ok := types.Unalias(underlying(t)).(*types.Struct)
		if !ok {
			return
		}
		for i := range st.NumFields() {
			f := st.Field(i)
			// A hidden field is not offered at all, rather than offered under
			// its Go name: the engine's Input[T] reads the tag the same way,
			// and a field falls to a resolver when the author does want it
			// served. On an embedded struct the tag hides everything it would
			// promote.
			tag, hidden := jsonName(st.Tag(i))
			if f.Embedded() {
				if !hidden {
					walk(f.Type(), depth+1)
				}
				continue
			}
			if !f.Exported() {
				continue
			}
			key := strings.ToLower(f.Name())
			if prev, taken := out[key]; taken && prev.deep <= depth {
				if prev.deep == depth {
					prev.ambiguous = true
					out[key] = prev
				}
				continue
			}
			// A hidden field is recorded like any other and removed below, so
			// that it shadows a deeper field of its name as it does in Go:
			// v.Secret is the outer one, and offering the embedded one would
			// bind to the value the tag hides.
			out[key] = goField{name: f.Name(), tag: tag, typ: f.Type(), deep: depth, hidden: hidden}
		}
	}
	walk(named, 0)
	for key, f := range out {
		if f.ambiguous || f.hidden {
			delete(out, key)
		}
	}
	return out
}

func underlying(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	return types.Unalias(t).Underlying()
}

// jsonName is the name a struct tag gives a field for JSON, and whether the
// tag hides the field altogether: json:"-", which is encoding/json's "never on
// the wire" and how an ORM marks a sensitive column. json:"-," is its escape
// for a field really named "-", and names it.
func jsonName(tag string) (name string, hidden bool) {
	v := reflect.StructTag(tag).Get("json")
	if v == "-" {
		return "", true
	}
	name, _, _ = strings.Cut(v, ",")
	return name, false
}

// matchField resolves an SDL field name to a Go field: an exact json tag
// first, since it was written deliberately, then a case-insensitive name.
func matchField(fields map[string]goField, sdlName string) (goField, bool) {
	// In key order: two fields can carry one tag, and ranging the map picked
	// a different one of them from one generate to the next.
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if f := fields[key]; f.tag != "" && f.tag == sdlName {
			return f, true
		}
	}
	f, ok := fields[strings.ToLower(sdlName)]
	return f, ok
}

// goMethod is a method with the shape a binding needs to declare.
type goMethod struct {
	name    string
	params  []types.Type // parameters after an optional leading context
	result  types.Type
	context bool
	errors  bool
	usable  bool
}

// fits reports whether the method can answer the field's arguments. The
// generator spreads them into the call as a.Name, with no conversion, so the
// count must agree and every parameter must be exactly the Go type that
// argument's field on the generated struct holds. Checking only the count
// bound an ORM edge method taking its own *ent.XOrder to an argument the
// generator models itself, which is a compile error in generated code.
func (m goMethod) fits(args ast.ArgumentDefinitionList, want func(*ast.Type, string, bool) string, qual types.Qualifier) bool {
	if !m.usable || len(m.params) != len(args) {
		return false
	}
	for i, arg := range args {
		if types.TypeString(m.params[i], qual) != want(arg.Type, "", false) {
			return false
		}
	}
	return true
}

// methodSet collects the methods of *T, which includes those promoted from
// embedded types.
func methodSet(named *types.Named) map[string]goMethod {
	out := map[string]goMethod{}
	ms := types.NewMethodSet(types.NewPointer(named))
	for i := range ms.Len() {
		sel := ms.At(i)
		fn, ok := sel.Obj().(*types.Func)
		if !ok || !fn.Exported() {
			continue
		}
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			continue
		}
		out[strings.ToLower(fn.Name())] = describe(fn.Name(), sig)
	}
	return out
}

// describe reads a signature into the shape a FieldBinding declares. Anything
// outside (ctx?, args...) (R) or (R, error) is not usable, and the field falls
// through to the resolver rather than generating a call that will not compile.
func describe(name string, sig *types.Signature) goMethod {
	m := goMethod{name: name}

	params := sig.Params()
	start := 0
	if params.Len() > 0 && isContext(params.At(0).Type()) {
		m.context = true
		start = 1
	}
	if sig.Variadic() {
		return m
	}
	for i := start; i < params.Len(); i++ {
		if isContext(params.At(i).Type()) {
			return m // a context anywhere but first is not a shape we emit
		}
		m.params = append(m.params, params.At(i).Type())
	}

	results := sig.Results()
	switch results.Len() {
	case 1:
	case 2:
		if !isError(results.At(1).Type()) {
			return m
		}
		m.errors = true
	default:
		return m
	}
	m.result = results.At(0).Type()
	m.usable = true
	return m
}

func matchMethod(methods map[string]goMethod, sdlName string) (goMethod, bool) {
	m, ok := methods[strings.ToLower(sdlName)]
	return m, ok
}

func isContext(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "context" && obj.Name() == "Context"
}

func isError(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() == nil && named.Obj().Name() == "error"
}
