// Package codegen generates models, argument structs, a Resolver interface
// and bindings from SDL.
package codegen

import (
	"fmt"
	"go/types"
	"reflect"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"golang.org/x/tools/go/packages"
)

// Auto-bind discovers bindings from Go types instead of being told them. It
// produces a Manifest and then stops: everything downstream is the manifest
// path, already built and tested, so discovery is the only new behaviour.
//
// Only the listed packages are loaded, and only their export data — no syntax
// trees and no function bodies. That is the difference from loading a whole
// module to bind a schema, which is the cost this project exists to avoid.

// loadMode is export data only. NeedSyntax or NeedTypesInfo would parse every
// file of every package, which is precisely what must not happen here.
const loadMode = packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps

// autoBind loads the given package patterns and infers a manifest from them.
func autoBind(dir string, patterns []string, schema *ast.Schema, want func(*ast.Type, string, bool) string) (*Manifest, error) {
	pkgs, err := packages.Load(&packages.Config{Dir: dir, Mode: loadMode}, patterns...)
	if err != nil {
		return nil, fmt.Errorf("auto-bind: load: %w", err)
	}
	var loadErrs []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	}
	if len(loadErrs) > 0 {
		slices.Sort(loadErrs)
		return nil, fmt.Errorf("auto-bind: %s", strings.Join(loadErrs, "\n"))
	}

	// Later patterns lose to earlier ones, so the order given is the
	// precedence: a schema type found in two packages binds to the first.
	found := map[string]*types.Named{}
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			obj, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || !obj.Exported() {
				continue
			}
			named, ok := types.Unalias(obj.Type()).(*types.Named)
			if !ok {
				continue
			}
			if _, taken := found[name]; !taken {
				found[name] = named
			}
		}
	}

	man := &Manifest{}
	for _, name := range objectNames(schema) {
		named, ok := found[name]
		if !ok {
			continue
		}
		tb := TypeBinding{
			Name:   name,
			Go:     GoType{PkgPath: named.Obj().Pkg().Path(), Name: named.Obj().Name()},
			Fields: discoverFields(schema.Types[name], named, want),
		}
		man.Types = append(man.Types, tb)
	}
	if len(man.Types) == 0 {
		return nil, nil
	}
	return man, nil
}

// objectNames lists the schema's object types, roots excluded: a root has no
// value behind it, so there is nothing to discover.
func objectNames(schema *ast.Schema) []string {
	var out []string
	for name, def := range schema.Types {
		if def.Kind != ast.Object || def.BuiltIn || strings.HasPrefix(name, "__") {
			continue
		}
		if (schema.Query != nil && schema.Query.Name == name) ||
			(schema.Mutation != nil && schema.Mutation.Name == name) ||
			(schema.Subscription != nil && schema.Subscription.Name == name) {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

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
func discoverFields(def *ast.Definition, named *types.Named, want func(*ast.Type, string, bool) string) map[string]FieldBinding {
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
				if conv, usable := reconcile(f.typ, expected); usable {
					out[fd.Name] = FieldBinding{Kind: FieldStruct, GoName: f.name, Convert: conv}
					continue
				}
				if conv, usable := reconcileValue(f.typ, expected); usable {
					out[fd.Name] = FieldBinding{Kind: FieldStruct, GoName: f.name, Convert: conv, Value: true}
					continue
				}
			}
		}
		if m, ok := matchMethod(methods, fd.Name); ok && m.fits(len(fd.Arguments)) {
			if conv, usable := reconcile(m.result, expected); usable {
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
}

// structFields collects the fields of a struct, following embedded ones. A
// shallower field wins, which is the promotion rule Go itself uses.
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
			if f.Embedded() {
				walk(f.Type(), depth+1)
				continue
			}
			if !f.Exported() {
				continue
			}
			key := strings.ToLower(f.Name())
			if prev, taken := out[key]; taken && prev.deep <= depth {
				continue
			}
			out[key] = goField{name: f.Name(), tag: jsonName(st.Tag(i)), typ: f.Type(), deep: depth}
		}
	}
	walk(named, 0)
	return out
}

func underlying(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	return types.Unalias(t).Underlying()
}

func jsonName(tag string) string {
	v := reflect.StructTag(tag).Get("json")
	if i := strings.Index(v, ","); i >= 0 {
		v = v[:i]
	}
	if v == "-" {
		return ""
	}
	return v
}

// matchField resolves an SDL field name to a Go field: an exact json tag
// first, since it was written deliberately, then a case-insensitive name.
func matchField(fields map[string]goField, sdlName string) (goField, bool) {
	for _, f := range fields {
		if f.tag != "" && f.tag == sdlName {
			return f, true
		}
	}
	f, ok := fields[strings.ToLower(sdlName)]
	return f, ok
}

// goMethod is a method with the shape a binding needs to declare.
type goMethod struct {
	name    string
	params  int // parameters after an optional leading context
	result  types.Type
	context bool
	errors  bool
	usable  bool
}

// fits reports whether the method can answer a field with n arguments. The
// generator spreads arguments into the call, so the counts must agree.
func (m goMethod) fits(n int) bool { return m.usable && m.params == n }

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
	m.params = params.Len() - start
	for i := start; i < params.Len(); i++ {
		if isContext(params.At(i).Type()) {
			return m // a context anywhere but first is not a shape we emit
		}
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

// mergeManifests lets an explicit manifest override discovery. Discovery is a
// guess from names; a written binding is a statement, so the statement wins,
// field by field rather than whole types.
func mergeManifests(discovered, explicit *Manifest) *Manifest {
	if discovered == nil {
		return explicit
	}
	if explicit == nil {
		return discovered
	}
	out := &Manifest{}
	byName := map[string]int{}
	for _, tb := range discovered.Types {
		byName[tb.Name] = len(out.Types)
		out.Types = append(out.Types, tb)
	}
	for _, tb := range explicit.Types {
		i, ok := byName[tb.Name]
		if !ok {
			out.Types = append(out.Types, tb)
			continue
		}
		merged := out.Types[i]
		if !tb.Go.zero() {
			merged.Go = tb.Go
		}
		if tb.Group != "" {
			merged.Group = tb.Group
		}
		if len(tb.Fields) > 0 {
			fields := map[string]FieldBinding{}
			for k, v := range merged.Fields {
				fields[k] = v
			}
			for k, v := range tb.Fields {
				fields[k] = v
			}
			merged.Fields = fields
		}
		out.Types[i] = merged
	}
	return out
}

// reconcile decides whether a Go type can answer a field needing the Go type
// spelled expected, and whether a conversion is required to do it.
//
// A named type and its underlying basic type are distinct to Go but the same
// value to a schema: an ORM storing an id as a plain string still answers an
// ID! field, and refusing that would send the single most common field in any
// schema through a resolver. A conversion is emitted only when both sides bottom
// out in the same basic kind, so it can neither lose information nor reinterpret
// one thing as another. Anything else is left to the resolver.
// reconcileValue is reconcile for a nullable SDL position: a Go value that is
// always present satisfies it, so "*float64 expected, float64 found" is a
// match and the field is emitted returning the value type. Only the pointer
// is dropped -- the pointee still has to reconcile.
func reconcileValue(actual types.Type, expected string) (convert, usable bool) {
	if !strings.HasPrefix(expected, "*") {
		return false, false
	}
	return reconcile(actual, expected[1:])
}

func reconcile(actual types.Type, expected string) (convert, usable bool) {
	if actual == nil || expected == "" {
		return false, false
	}
	if types.TypeString(actual, packageNameQualifier) == expected {
		return false, true
	}
	want, ok := basicClassOf(expected)
	if !ok {
		return false, false
	}
	basic, ok := types.Unalias(actual).Underlying().(*types.Basic)
	if !ok {
		return false, false
	}
	return true, basic.Info()&want != 0
}

// basicClassOf maps a Go type expression the generator emits to the class of
// basic type it bottoms out in. Only leaf types appear here; a list or a
// pointer has no class and is required to match exactly.
func basicClassOf(expr string) (types.BasicInfo, bool) {
	switch expr {
	case "string", "graphql.ID":
		return types.IsString, true
	case "int", "int32", "int64":
		return types.IsInteger, true
	case "float64", "float32":
		return types.IsFloat, true
	case "bool":
		return types.IsBoolean, true
	}
	return 0, false
}

// packageNameQualifier renders types the way generated code spells them, by
// package name rather than by import path.
func packageNameQualifier(p *types.Package) string { return p.Name() }
