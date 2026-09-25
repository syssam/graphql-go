// Package codegen generates models, argument structs, a Resolver interface
// and bindings from SDL.
package codegen

import (
	"fmt"
	"go/constant"
	"go/types"
	"maps"
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
func autoBind(dir string, patterns []string, schema *ast.Schema, b *builder) (*Manifest, error) {
	b.loads++
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

	// Discovery is two passes, and the order is load-bearing. Whether a Go
	// field can answer a GraphQL field is decided by comparing it against the
	// Go type that field needs, and that type is only known once every type
	// this pass binds is in the model map. Deciding fields as the types are
	// discovered measures each one against the model the generator would have
	// written instead: Product.owner is *ent.Owner against *model.Owner, so
	// every object-valued and every enum-valued field falls through to the
	// Resolver however plainly it is a struct field.
	all := slices.Concat(pkgs, modelPkgs(dir, schema, b.cfg.Models, pkgs, &b.loads))
	idx := namedIndex(all)

	// A declared binding the loaded packages show cannot work is dropped
	// before anything reads it, so the enum is modelled by the generator and
	// every field of it agrees.
	dropped, marshalers := unbindableTypes(schema, b.cfg.Models, idx)
	b.marshalers = marshalers
	if len(dropped) > 0 {
		kept := maps.Clone(b.cfg.Models)
		for _, name := range dropped {
			delete(kept, name)
		}
		b.setModels(kept)
		// One line, not one per enum: the real schema drops 444 of them, and
		// 444 lines of stderr is a wall nobody reads to the end of.
		b.notef("%d bindings dropped: the declared Go type cannot back an enum, or holds one that cannot, so a model is generated instead (%s)",
			len(dropped), summarize(dropped, 8))
	}

	man := &Manifest{}
	man.Types = append(man.Types, enumBindings(schema, b.cfg.Models, found, constIndex(all))...)

	// A second Go type for one SDL enum, found through a bound input struct.
	// It has to be discovered after the drop pass above, so that an enum whose
	// declared type was dropped is not compared against a type nothing binds.
	if alt := altEnumTypes(schema, b.cfg.Models, found, idx); len(alt) > 0 {
		withAlt := slices.Concat(all, altEnumPkgs(dir, alt, all, &b.loads))
		man.ExtraEnums = altEnumBindings(schema, alt, constIndex(withAlt), b.notef)
	}
	objects := objectNames(schema)
	for _, name := range objects {
		named, ok := found[name]
		if !ok {
			continue
		}
		man.Types = append(man.Types, TypeBinding{
			Name: name,
			Go:   GoType{PkgPath: named.Obj().Pkg().Path(), Name: named.Obj().Name()},
		})
	}

	// A declared binding still wins -- this only fills in what discovery
	// found and nothing else says.
	models := maps.Clone(b.cfg.Models)
	if models == nil {
		models = map[string]string{}
	}
	for _, tb := range man.Types {
		if _, declared := models[tb.Name]; !declared && !tb.Go.zero() {
			models[tb.Name] = tb.Go.expr()
		}
	}
	b.setModels(models)

	// Fields are read off the Go type the schema will actually use, which
	// is the declared one wherever a declaration exists. Reading them off
	// the type found by name instead binds what that other type has: two
	// packages each holding a PaymentTerms, one with a
	// HasEarlyPaymentDiscount method and one without, produced a field
	// calling a method the declared type does not have. A declared type
	// this pass did not load yields no fields at all, so its fields are
	// resolvers -- a method that can always be written, rather than a guess
	// that is a compile error in code the author did not write.
	for i := range man.Types {
		name := man.Types[i].Name
		if schema.Types[name].Kind != ast.Object {
			continue
		}
		named := found[name]
		if expr, declared := b.cfg.Models[name]; declared {
			named = namedByExpr(idx, expr)
		}
		if named == nil {
			continue
		}
		man.Types[i].Fields = discoverFields(schema.Types[name], named, b.goType, b.goQualifier)
	}
	if len(man.Types) == 0 {
		return nil, nil
	}
	return man, nil
}

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

// modelPkgs loads the packages the model map names for an enum or an input
// object and that the auto-bind patterns did not already reach.
//
// An ORM declares an enum's Go type beside the entity that uses it, so a
// schema with five hundred entities names five hundred packages -- and the
// config already names every one of them, in the Models entry or the @goModel
// directive that binds the enum. Making the author list them a second time
// under AutoBind is bookkeeping, and getting it wrong is silent: the constant
// name is derived instead, which is a compile error in generated code.
//
// Nothing here widens discovery. No type found in one of these packages is
// bound to a schema type -- an author who wanted that would have auto-bound
// it. They are read for their constants, and for the shape of a type the
// config already declared, which is what makes it possible to say that a
// declared binding will not compile.
//
// Input objects are here for the second reason and not the first. Their fields
// are derived by the engine, not emitted, so nothing needs their constants --
// but a filter struct is where an ORM's second Go type for an enum is found
// (altEnumTypes), and a declared input type this pass never loaded has no
// fields to look at.
func modelPkgs(dir string, schema *ast.Schema, models map[string]string, loaded []*packages.Package, loads *int) []*packages.Package {
	have := map[string]bool{}
	for _, p := range loaded {
		have[p.PkgPath] = true
	}
	var want []string
	seen := map[string]bool{}
	for _, name := range slices.Concat(enumNames(schema), inputNames(schema), scalarNames(schema)) {
		path, _ := splitModelExpr(models[name])
		if path == "" || have[path] || seen[path] {
			continue
		}
		seen[path] = true
		want = append(want, path)
	}
	if len(want) == 0 {
		return nil
	}
	// A path that does not load is not an error: the model map may name a
	// package outside this module, and the enum then keeps the behaviour it
	// had before constants were discovered at all.
	*loads++
	pkgs, err := packages.Load(&packages.Config{Dir: dir, Mode: loadMode}, want...)
	if err != nil {
		return nil
	}
	return pkgs
}

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

// inputNames lists the schema's input object types, sorted.
func inputNames(schema *ast.Schema) []string {
	var out []string
	for name, def := range schema.Types {
		if def.Kind == ast.InputObject && !def.BuiltIn && !strings.HasPrefix(name, "__") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// scalarNames lists the schema's custom scalars, sorted.
func scalarNames(schema *ast.Schema) []string {
	var out []string
	for name, def := range schema.Types {
		if def.Kind == ast.Scalar && !def.BuiltIn && !strings.HasPrefix(name, "__") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// enumNames lists the schema's enum types, sorted.
func enumNames(schema *ast.Schema) []string {
	var out []string
	for name, def := range schema.Types {
		if def.Kind == ast.Enum && !def.BuiltIn && !strings.HasPrefix(name, "__") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
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
	// Discovery is the only producer of these, and an explicit manifest may
	// add its own: an author who knows about a second Go type says so here
	// rather than editing generated code.
	out.ExtraEnums = slices.Concat(discovered.ExtraEnums, explicit.ExtraEnums)
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
func reconcileValue(actual types.Type, expected string, qual types.Qualifier) (convert, usable bool) {
	if !strings.HasPrefix(expected, "*") {
		return false, false
	}
	return reconcile(actual, expected[1:], qual)
}

func reconcile(actual types.Type, expected string, qual types.Qualifier) (convert, usable bool) {
	if actual == nil || expected == "" {
		return false, false
	}
	if types.TypeString(actual, qual) == expected {
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

// goQualifier renders a loaded type's package the way generated code spells
// it, so a Go type and the expression the generator wants can be compared as
// strings. A mapped package is spelled with the qualifier modelExprImports gave
// it, which need not be its name; any other by its name, unless that name is a
// mapped package's qualifier, when two different types would read alike.
func (b *builder) goQualifier(p *types.Package) string {
	imports := b.modelExprImports()
	if q, ok := b.exprQualifiers[p.Path()]; ok {
		return q
	}
	if path, taken := imports[p.Name()]; taken && path != p.Path() {
		return p.Path()
	}
	return p.Name()
}

// namedIndex maps "importPath.TypeName" to the named type, over every package
// that was loaded.
func namedIndex(pkgs []*packages.Package) map[string]*types.Named {
	out := map[string]*types.Named{}
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			obj, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			if named, ok := types.Unalias(obj.Type()).(*types.Named); ok && named.Obj().Pkg() != nil {
				out[named.Obj().Pkg().Path()+"."+named.Obj().Name()] = named
			}
		}
	}
	return out
}

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

// namedByExpr resolves a Config.Models entry to the loaded named type it
// spells, or nil when that package was not loaded.
func namedByExpr(idx map[string]*types.Named, expr string) *types.Named {
	path, _ := splitModelExpr(expr)
	if path == "" {
		return nil
	}
	return idx[path+expr[strings.LastIndex(expr, "."):]]
}

// summarize renders at most n of the names, with a count of the rest.
func summarize(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:n], ", "), len(names)-n)
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
