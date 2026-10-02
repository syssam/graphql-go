package codegen

import (
	"fmt"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// wireDeps is Config.Wire.Deps taken apart: the package it is imported from,
// the qualifier generated code refers to it under, and the type's name.
func (b *builder) wireDeps() (importPath, qualifier, typeName string) {
	importPath, ref := splitModelExpr(b.cfg.Wire.Deps)
	qualifier, typeName, _ = strings.Cut(ref, ".")
	return importPath, qualifier, typeName
}

// constructorName is the function Wire calls to build an implementation:
// New and the type's name, as the scaffold writes it.
func constructorName(typ string) string {
	r, size := utf8.DecodeRuneInString(typ)
	return "New" + string(unicode.ToUpper(r)) + typ[size:]
}

// importPathOf is the import path of a directory, worked out from where
// Output is and what it is imported as. Scaffold targets are named by
// directory, and the generated Wire has to import them.
func (b *builder) importPathOf(dir string) (string, error) {
	rel, err := filepath.Rel(outputDir(b.dir, b.cfg.Output), dir)
	if err != nil {
		return "", err
	}
	return path.Clean(b.cfg.Package + "/" + filepath.ToSlash(rel)), nil
}

// wireNames are identifiers wire.go cannot use for an import: the engine's
// package, the function's own parameter, and what the root package declares.
var wireNames = map[string]bool{
	"graphql": true, "d": true, "sdl": true, "fed": true, "embed": true, "context": true,
	"Wire": true, "NewSchema": true, "ValidateSchema": true, "placeholderEntities": true,
}

// emitWire renders wire.go: every group's bindings over the Resolver its
// constructor builds from one dependencies value.
//
// It needs nothing scaffolding has not been told -- the group, and where its
// implementation is -- so it is written with the rest of the output, before
// scaffolding adds a constructor to an implementation that lacks one.
func (b *builder) emitWire(groups []string) (string, error) {
	depsPath, depsQual, depsType := b.wireDeps()
	flat := len(groups) <= 1
	outDir := outputDir(b.dir, b.cfg.Output)

	// One qualifier per import path, always written as an alias: a package
	// need not be named after its directory, and two directories may share a
	// last element.
	qualifiers := map[string]string{}
	taken := map[string]bool{}
	qualify := func(importPath, preferred string) string {
		if q, ok := qualifiers[importPath]; ok {
			return q
		}
		q := preferred
		for n := 2; taken[q] || wireNames[q]; n++ {
			q = fmt.Sprintf("%s%d", preferred, n)
		}
		qualifiers[importPath], taken[q] = q, true
		return q
	}
	// The dependencies type may be declared in the generated package itself,
	// which a file in that package names without importing anything. With
	// one group that is where everything is. With several it cannot work:
	// each group's implementation would import the root for the type, and
	// the root imports every one of them to call its constructor.
	depsRef := depsType
	switch {
	case depsPath != b.cfg.Package:
		depsRef = qualify(depsPath, depsQual) + "." + depsType
	case !flat:
		return "", fmt.Errorf("codegen: wire: %s is declared in the generated root package, which imports every group's implementation, so none of them can import it for the type; declare it in a package of its own", b.cfg.Wire.Deps)
	}

	type line struct{ bindings, constructor string }
	var lines []line
	var missing []string
	add := func(name, group string) error {
		if !b.hasResolver(group) {
			return nil
		}
		target, ok := b.cfg.Scaffold[name]
		if !ok {
			missing = append(missing, name)
			return nil
		}
		dir, typ, err := b.splitTarget(target)
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		groupDir, groupImport := outDir, b.cfg.Package
		if !flat {
			groupDir, groupImport = filepath.Join(outDir, filepath.FromSlash(b.groupRel(group))), b.groupImport(group)
		}
		implImport := groupImport
		if !samePath(dir, groupDir) {
			if implImport, err = b.importPathOf(dir); err != nil {
				return fmt.Errorf("codegen: wire: %s: %w", target, err)
			}
			// With one group the generated package is the group's own, and an
			// implementation outside it imports it for the Resolver's types.
			if flat {
				return fmt.Errorf("codegen: wire: %s is outside the generated package and imports it, so a Wire inside that package would import it back; pass %s.Bindings(...) to NewSchema from your own code instead", target, b.pkgName)
			}
		}
		bindings, constructor := "Bindings", constructorName(typ)
		if !flat {
			bindings = qualify(groupImport, identFrom(path.Base(groupImport))) + "." + bindings
		}
		if implImport != b.cfg.Package {
			constructor = qualify(implImport, identFrom(path.Base(implImport))) + "." + constructor
		}
		lines = append(lines, line{bindings, constructor})
		return nil
	}
	if flat {
		if err := add(b.pkgName, ""); err != nil {
			return "", err
		}
	}
	for _, g := range groups {
		if flat {
			break
		}
		if err := add(g, g); err != nil {
			return "", err
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return "", fmt.Errorf("codegen: wire: %s: a group with a Resolver needs a scaffold target, which is where Wire finds the constructor to call", strings.Join(missing, ", "))
	}

	// One type may implement several groups. It is then one value, built once
	// and handed to each, as anyone wiring it by hand would write it: a call
	// per group made an instance per group, each with its own state.
	uses := map[string]int{}
	for _, l := range lines {
		uses[l.constructor]++
	}
	shared := map[string]string{}
	var sharedOrder []string
	for _, l := range lines {
		if uses[l.constructor] < 2 || shared[l.constructor] != "" {
			continue
		}
		// Named after the type, and never after anything already in scope: a
		// variable called resolver would hide the package resolver for every
		// line below it.
		typ := l.constructor[strings.LastIndex(l.constructor, ".")+1+len("New"):]
		r, size := utf8.DecodeRuneInString(typ)
		preferred := string(unicode.ToLower(r)) + typ[size:]
		if token.IsKeyword(preferred) || types.Universe.Lookup(preferred) != nil {
			preferred += "Impl"
		}
		name := preferred
		for n := 2; taken[name] || wireNames[name]; n++ {
			name = fmt.Sprintf("%s%d", preferred, n)
		}
		taken[name] = true
		shared[l.constructor] = name
		sharedOrder = append(sharedOrder, l.constructor)
	}

	var w strings.Builder
	w.WriteString(b.header(b.pkgName, ""))
	w.WriteString("import (\n\t\"github.com/syssam/graphql-go\"\n\n")
	for _, importPath := range slices.Sorted(func(yield func(string) bool) {
		for p := range qualifiers {
			if !yield(p) {
				return
			}
		}
	}) {
		fmt.Fprintf(&w, "\t%s %q\n", qualifiers[importPath], importPath)
	}
	w.WriteString(")\n\n")
	fmt.Fprintf(&w, `// Wire builds every group's Resolver from d and returns their bindings, to
// pass to NewSchema. Nothing lists the groups by hand and nothing is resolved
// when the process starts: each line calls the constructor beside a group's
// implementation, and a group whose constructor is missing, or takes
// something else, does not compile.
func Wire(d %s) []graphql.SchemaOption {
`, depsRef)
	for _, constructor := range sharedOrder {
		fmt.Fprintf(&w, "\t%s := %s(d)\n", shared[constructor], constructor)
	}
	w.WriteString("\treturn []graphql.SchemaOption{\n")
	for _, l := range lines {
		if name := shared[l.constructor]; name != "" {
			fmt.Fprintf(&w, "\t\t%s(%s),\n", l.bindings, name)
			continue
		}
		fmt.Fprintf(&w, "\t\t%s(%s(d)),\n", l.bindings, l.constructor)
	}
	w.WriteString("\t}\n}\n")
	return w.String(), nil
}
