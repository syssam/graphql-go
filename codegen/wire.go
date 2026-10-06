package codegen

import (
	"fmt"
	"go/token"
	"go/types"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

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
	if b.readWire(); b.wire.generic {
		return "", fmt.Errorf("codegen: wire: %s is generic, and Wire has to take one type: declare a type for the instantiation the application uses and name that", b.cfg.Wire.Deps)
	}
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

	type line struct{ name, bindings, constructor, arg string }
	var lines []line
	builtBy := map[string]string{}
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
		if dir, err = b.wireDir(dir); err != nil {
			return err
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
		arg, err := b.wireCall(name, target, dir, typ, implImport)
		if err != nil {
			return err
		}
		// Two types in one package whose names differ only in their first
		// letter's case are both built by one function, which returns one.
		if other, ok := builtBy[constructor]; ok && other != typ {
			return fmt.Errorf("codegen: wire: %s and %s in one package are both built by a function called %s, which can return only one of them; rename one", other, typ, constructorName(typ))
		}
		builtBy[constructor] = typ
		lines = append(lines, line{name, bindings, constructor, arg})
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
	// An entry for a group there is none of is a misspelling, and would
	// otherwise be the one entry that silently does nothing.
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Wire.Pass)) {
		if !slices.ContainsFunc(lines, func(l line) bool { return l.name == name }) {
			var have []string
			for _, l := range lines {
				have = append(have, l.name)
			}
			return "", fmt.Errorf("codegen: wire.pass: %s is not a group with a Resolver; those are %s", name, strings.Join(have, ", "))
		}
	}

	// One type may implement several groups. It is then one value, built once
	// and handed to each, as anyone wiring it by hand would write it: a call
	// per group made an instance per group, each with its own state.
	uses := map[string]int{}
	for _, l := range lines {
		uses[l.constructor]++
	}
	shared, sharedArg := map[string]string{}, map[string]string{}
	var sharedOrder []string
	for _, l := range lines {
		if uses[l.constructor] < 2 {
			continue
		}
		// One value serves every group. They are all given the same thing:
		// left to themselves they are, and wirePass has one entry speak for
		// every group a type implements.
		if _, seen := sharedArg[l.constructor]; seen {
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
		shared[l.constructor], sharedArg[l.constructor] = name, l.arg
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
// something else, does not compile. A constructor is given what its
// parameters are named after -- a field of d each, or nothing -- and
// otherwise the field named after its group, where there is one, so its
// package imports that and no more; an entry in the configuration's
// wire.pass decides instead, for every group its type implements.
func Wire(d %s) []graphql.SchemaOption {
`, depsRef)
	for _, constructor := range sharedOrder {
		fmt.Fprintf(&w, "\t%s := %s(%s)\n", shared[constructor], constructor, sharedArg[constructor])
	}
	w.WriteString("\treturn []graphql.SchemaOption{\n")
	for _, l := range lines {
		if name := shared[l.constructor]; name != "" {
			fmt.Fprintf(&w, "\t\t%s(%s),\n", l.bindings, name)
			continue
		}
		fmt.Fprintf(&w, "\t\t%s(%s(%s)),\n", l.bindings, l.constructor, l.arg)
	}
	w.WriteString("\t}\n}\n")
	return w.String(), nil
}
