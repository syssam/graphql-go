package codegen

import (
	"fmt"
	"go/types"
	"maps"
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
	var unread []string
	for _, name := range objects {
		named, ok := found[name]
		if !ok {
			// Not found by name, and bound by name all the same: the config
			// says which Go type, so its fields are read as a discovered
			// type's are. Left to the emitter they are derived from the SDL,
			// url as Url against a field called URL.
			if named = namedByExpr(idx, b.cfg.Models[name]); named == nil {
				if path, _ := splitModelExpr(b.cfg.Models[name]); path != "" {
					unread = append(unread, name)
				}
				continue
			}
		}
		man.Types = append(man.Types, TypeBinding{
			Name: name,
			Go:   GoType{PkgPath: named.Obj().Pkg().Path(), Name: named.Obj().Name()},
		})
	}

	if len(unread) > 0 {
		// Said, because the next generate may answer differently: a struct
		// that holds this run's generated types builds only after it.
		b.notef("%d object(s) are bound to a Go type that could not be read, so their fields are written from the SDL's names rather than checked against the struct: %s. The package does not build yet or is not in this module; generate again once it builds.",
			len(unread), summarize(unread, 8))
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

// modelPkgs loads the packages the model map names for an enum, an input
// object, a scalar or an object and that the auto-bind patterns did not
// already reach.
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
//
// Objects are here for the second reason too. An object bound to a struct in
// a package no pattern names -- a service's own result type -- had its fields
// written from the SDL's names, and a field the struct spells differently, or
// does not have, was a compile error in generated code.
func modelPkgs(dir string, schema *ast.Schema, models map[string]string, loaded []*packages.Package, loads *int) []*packages.Package {
	have := map[string]bool{}
	for _, p := range loaded {
		have[p.PkgPath] = true
	}
	var want []string
	seen := map[string]bool{}
	for _, name := range slices.Concat(enumNames(schema), inputNames(schema), scalarNames(schema), objectNames(schema)) {
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
