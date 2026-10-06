package codegen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// scaffoldGroup brings one implementation up to its group's interface, and
// returns the interface's method names. target is "dir.Type", dir relative
// to Config.Dir. pkgs holds each target directory parsed once for the whole
// run, since every group may share one.
func (b *builder) scaffoldGroup(pkgs *pkgCache, name, group, target string) ([]string, error) {
	dir, typ, err := b.splitTarget(target)
	if err != nil {
		return nil, err
	}
	idx, err := pkgs.get(b, dir)
	if err != nil {
		return nil, err
	}
	have, declared, pkg := idx.methods[typ], idx.declared[typ], idx.pkg
	if pkg == "" {
		pkg = identFrom(filepath.Base(dir))
	}

	groupImport := b.cfg.Package
	groupDir := outputDir(b.dir, b.cfg.Output)
	if name != b.pkgName || group != "" {
		groupImport = b.groupImport(group)
		groupDir = filepath.Join(groupDir, filepath.FromSlash(b.groupRel(group)))
	}
	// A name the generator declares in the target package -- the group's own,
	// or any other group's -- is refused rather than redeclared, or given
	// methods, inside a file marked DO NOT EDIT.
	if idx.generated[typ] {
		return nil, fmt.Errorf("target %q: %s is declared by the generated code in that package; name the implementation something else", target, typ)
	}
	// In the group's own package the group is not imported -- it is the
	// package -- and its names are not qualified: importing itself is a cycle.
	self := samePath(dir, groupDir)
	alias := identFrom(name) + "gql"
	qual := alias + "."
	if self {
		qual = ""
	}
	sigs := b.resolverSignatures(group, qual)
	names := make([]string, len(sigs))
	for i, m := range sigs {
		names[i] = m.name
	}
	var stubs strings.Builder
	for _, m := range sigs {
		if have[m.name] {
			continue
		}
		// Name first, then the description, as the interface has it: the stub
		// is the author's code from now on, and a doc comment that does not
		// open with the method's name fails ST1020 there.
		fmt.Fprintf(&stubs, "\n// %s resolves %s.\n", m.name, m.coord)
		if doc := strings.TrimSpace(m.doc); doc != "" {
			stubs.WriteString("//\n")
			for line := range strings.SplitSeq(doc, "\n") {
				stubs.WriteString(strings.TrimRight("// "+line, " ") + "\n")
			}
		}
		fmt.Fprintf(&stubs, "func (r *%s) %s%s {\n\tpanic(%q)\n}\n", typ, m.name, m.signature, "not implemented: "+m.coord)
	}
	file := filepath.Join(dir, identFrom(name)+".resolvers.go")
	old, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	imports, err := b.stubImports(stubs.String())
	if err != nil {
		return nil, err
	}

	// With Wire configured the implementation needs a constructor from the
	// dependencies, which Wire calls. A new type is given one that keeps
	// them, and one that is already there is left alone. A type that exists
	// without one is the author's, with fields this run knows nothing about:
	// a constructor written for it could only panic, which compiles and then
	// stops the process at start-up, the failure Wire exists to turn into a
	// compile error. So nothing is written, the build fails on the missing
	// name, and a note says what to write.
	fields, constructor := "{}", ""
	ctor := constructorName(typ)
	if in := idx.inTests[ctor]; in != "" && !b.cfg.Wire.IsZero() && !idx.funcs[ctor] && !idx.names[ctor] && !idx.wanted[ctor] {
		idx.wanted[ctor] = true
		b.notef("wire: %s is declared in %s, a test file, which is not in the build the generated Wire is part of; gqlc will not write a second one beside it. Move it to a file the build includes.",
			ctor, in)
	}
	if !b.cfg.Wire.IsZero() && !idx.funcs[ctor] && !idx.names[ctor] && idx.inTests[ctor] == "" {
		depsPath, depsQual, depsType := b.wireDeps()
		implImport, _ := b.importPathOf(dir)
		// Each package the constructor's parameter names is imported once,
		// under a name free in this file.
		chosen := map[string]string{}
		var qualifyErr error
		qualify := func(importPath, preferred string) string {
			if q, ok := chosen[importPath]; ok {
				return q
			}
			q, add, err := depsQualifier(old, importPath, preferred, idx, imports, alias, typ)
			if err != nil {
				qualifyErr = err
			}
			if add {
				imports = append(imports, importSpec{q, importPath})
			}
			chosen[importPath] = q
			return q
		}
		if declared {
			// The note names the type as the dependencies file writes it;
			// nothing is imported for a constructor that is not written.
			qualify = func(_, preferred string) string { return preferred }
		}
		// What the constructor takes: the field named after the group, or
		// the whole value. See wireArg.
		param, paramType, what := "d", depsType, "the application's dependencies"
		// Wire.Pass decides where it speaks, and has been checked by the
		// time anything is scaffolded; otherwise the struct does.
		pass, err := b.wirePass(name, target)
		if err != nil {
			return nil, err
		}
		var (
			field *depsField
			why   error
		)
		if pass != nil {
			field = pass.field
		} else {
			field, why = b.wireArg(name, target)
		}
		switch {
		case field != nil:
			placed, err := field.placed(depsPath, depsQual, implImport, qualify)
			if err != nil {
				return nil, fmt.Errorf("wire: %s.%s: %w", depsType, field.name, err)
			}
			param, paramType, what = scaffoldParam, placed, "the "+field.name+" field of the application's dependencies"
		case implImport != depsPath:
			paramType = qualify(depsPath, depsQual) + "." + depsType
		}
		if pass != nil && pass.pointer {
			paramType, what = "*"+paramType, "a pointer to "+what
		}
		if qualifyErr != nil {
			return nil, fmt.Errorf("%s: %w", file, qualifyErr)
		}
		switch {
		case declared:
			if !idx.wanted[ctor] {
				idx.wanted[ctor] = true
				where := dir
				if rel, err := filepath.Rel(b.dir, dir); err == nil {
					where = rel
				}
				b.notef("wire: %s has no %s, which the generated Wire calls, so the build fails until there is one. Write func %s(%s %s) *%s in %s.",
					target, ctor, ctor, param, paramType, typ, where)
			}
		default:
			keep := param
			if field == nil {
				keep = "deps"
				// Said once, by the run that writes the constructor, and
				// only where there is something to do about it: a field that
				// is there and could not be handed over, or a group left
				// without one where others have theirs, which usually means
				// the field is still to be added. A type serving several
				// groups takes the whole value by design.
				switch {
				case pass != nil:
					// Asked for.
				case why != nil:
					b.notef("wire: %v. %s takes the whole value instead, so its package imports everything that names.", why, ctor)
				// Only of a struct that was read: of one that was not, what
				// fields it has is not known.
				case b.wire.structRead && !b.wireShared(target) && b.wireAnyField():
					b.notef("wire: the %s group has no field named after it in %s, so %s takes the whole value and its package imports everything that names. Add the field and change the constructor to keep its builds apart.",
						name, depsType, ctor)
				}
			}
			fields = " {\n\t" + keep + " " + paramType + "\n}"
			constructor = fmt.Sprintf("\n// %s builds the %s group's Resolver from %s.\n// The generated Wire calls it.\nfunc %s(%s %s) *%s { return &%s{%s: %s} }\n",
				ctor, name, what, ctor, param, paramType, typ, typ, keep, param)
			idx.funcs[ctor] = true
		}
	}
	if stubs.Len() == 0 && declared {
		return names, nil
	}
	body := constructor + stubs.String()
	if !declared {
		body = fmt.Sprintf("\n// %s implements the %s group's Resolver.\ntype %s struct%s\n\nvar _ %sResolver = (*%s)(nil)\n",
			typ, name, typ, fields, qual, typ) + body
	}

	src := append(append([]byte{}, old...), body...)
	if !self && bytes.Contains(src, []byte(alias+".")) {
		imports = append([]importSpec{{alias, groupImport}}, imports...)
	}
	var out []byte
	if len(old) == 0 {
		// A new file is written whole, imports grouped the way goimports
		// would: the standard library, then everything else.
		out, err = format.Source([]byte("package " + pkg + "\n\n" + importDecl(imports) + body))
	} else {
		out, err = addImports(src, len(old), imports, b.packageName, idx.names)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, out, 0o644); err != nil {
		return nil, err
	}
	// What was just written is in the package now, for a later group that
	// shares it.
	idx.add(pkg, typ, sigs)
	return names, nil
}

// depsQualifier is the name a resolvers file refers to the dependencies
// package under, and whether the file still needs the import. A file that
// imports the package already keeps its own name for it. Otherwise the
// preferred name is used unless something in the file's scope has it: another
// import -- a group's model package is named after the group, and so may the
// dependencies package be -- or a name the package declares.
func depsQualifier(old []byte, depsPath, preferred string, idx *pkgIndex, imports []importSpec, alias, typ string) (qual string, add bool, err error) {
	taken := map[string]bool{alias: true, typ: true, "d": true}
	local := func(im importSpec) string {
		if im.name != "" {
			return im.name
		}
		// An import with no name is known by its package's, which is its last
		// path element by convention and by nothing gqlc can check.
		return identFrom(lastElement(im.path))
	}
	if len(old) > 0 {
		f, err := parser.ParseFile(token.NewFileSet(), "", old, parser.ImportsOnly)
		if err != nil {
			return "", false, err
		}
		for _, spec := range f.Imports {
			im := importSpec{path: strings.Trim(spec.Path.Value, "\"`")}
			if spec.Name != nil {
				im.name = spec.Name.Name
			}
			if im.path == depsPath && im.name != "_" && im.name != "." {
				return local(im), false, nil
			}
			taken[local(im)] = true
		}
	}
	for _, im := range imports {
		if im.path == depsPath {
			return local(im), false, nil
		}
		taken[local(im)] = true
	}
	for _, names := range []map[string]bool{idx.declared, idx.funcs, idx.generated, idx.names} {
		for n := range names {
			taken[n] = true
		}
	}
	qual = preferred
	for n := 2; taken[qual]; n++ {
		qual = fmt.Sprintf("%s%d", preferred, n)
	}
	return qual, true, nil
}
