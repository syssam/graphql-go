package codegen

import (
	"bytes"
	"fmt"
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
	t, err := b.resolveScaffoldTarget(pkgs, name, group, target)
	if err != nil {
		return nil, err
	}
	sigs := b.resolverSignatures(group, t.qual)
	names := make([]string, len(sigs))
	for i, m := range sigs {
		names[i] = m.name
	}
	stubs, written := renderStubs(t.typ, sigs, t.idx.methods[t.typ])
	old, err := os.ReadFile(t.file)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	imports, err := b.stubImports(stubs)
	if err != nil {
		return nil, err
	}
	fields, constructor, imports, err := b.scaffoldConstructor(t, old, imports)
	if err != nil {
		return nil, err
	}
	if stubs == "" && t.declared {
		return names, nil
	}
	body := constructor + stubs
	if !t.declared {
		body = fmt.Sprintf("\n// %s implements the %s group's Resolver.\ntype %s struct%s\n\nvar _ %sResolver = (*%s)(nil)\n",
			t.typ, t.name, t.typ, fields, t.qual, t.typ) + body
	}
	if err := b.writeScaffold(t, old, body, imports); err != nil {
		return nil, err
	}
	// What was just written is in the package now, for a later group that
	// shares it.
	t.idx.add(t.pkg, t.typ, sigs)
	t.idx.addStubs(t.typ, written)
	return names, nil
}

// scaffoldTarget is one Scaffold target resolved against its package: where
// the implementation lives, what is there already, and how the file refers
// to the group it implements.
type scaffoldTarget struct {
	name, target string
	dir, typ     string
	// pkg is the package clause the resolvers file needs.
	pkg string
	idx *pkgIndex
	// declared is whether the type already exists in the package.
	declared bool
	// self is a target inside the group's own package, which is neither
	// imported nor qualified there: importing itself is a cycle.
	self bool
	// alias is what the group's package is imported as, and qual is alias
	// plus a dot, or empty when self.
	alias, qual string
	groupImport string
	// file is the resolvers file scaffolding adds to.
	file string
}

func (b *builder) resolveScaffoldTarget(pkgs *pkgCache, name, group, target string) (*scaffoldTarget, error) {
	dir, typ, err := b.splitTarget(target)
	if err != nil {
		return nil, err
	}
	idx, err := pkgs.get(b, dir)
	if err != nil {
		return nil, err
	}
	pkg := idx.pkg
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
	t := &scaffoldTarget{
		name: name, target: target, dir: dir, typ: typ, pkg: pkg, idx: idx,
		declared: idx.declared[typ], self: samePath(dir, groupDir),
		alias: identFrom(name) + "gql", groupImport: groupImport,
		file: filepath.Join(dir, identFrom(name)+".resolvers.go"),
	}
	if !t.self {
		t.qual = t.alias + "."
	}
	return t, nil
}

// renderStubs writes a panicking method for each signature typ lacks, and
// returns them with the coordinates they answer.
func renderStubs(typ string, sigs []resolverSig, have map[string]bool) (string, []string) {
	var stubs strings.Builder
	var written []string
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
		fmt.Fprintf(&stubs, "func (r *%s) %s%s {\n\tpanic(%q)\n}\n", typ, m.name, m.signature, stubPanic+m.coord)
		written = append(written, m.coord)
	}
	return stubs.String(), written
}

// scaffoldConstructor is the type's struct body and constructor under Wire,
// and the imports they add to the file; "{}" and none without Wire.
//
// With Wire configured the implementation needs a constructor from the
// dependencies, which Wire calls. A new type is given one that keeps
// them, and one that is already there is left alone. A type that exists
// without one is the author's, with fields this run knows nothing about:
// a constructor written for it could only panic, which compiles and then
// stops the process at start-up, the failure Wire exists to turn into a
// compile error. So nothing is written, the build fails on the missing
// name, and a note says what to write.
func (b *builder) scaffoldConstructor(t *scaffoldTarget, old []byte, imports []importSpec) (fields, constructor string, _ []importSpec, _ error) {
	fields = "{}"
	if b.cfg.Wire.IsZero() {
		return fields, "", imports, nil
	}
	ctor := constructorName(t.typ)
	if in := t.idx.inTests[ctor]; in != "" && !t.idx.funcs[ctor] && !t.idx.names[ctor] && !t.idx.wanted[ctor] {
		t.idx.wanted[ctor] = true
		b.notef("wire: %s is declared in %s, a test file, which is not in the build the generated Wire is part of; gqlc will not write a second one beside it. Move it to a file the build includes.",
			ctor, in)
	}
	if !t.idx.funcs[ctor] && !t.idx.names[ctor] && t.idx.inTests[ctor] == "" {
		p, withParam, err := b.wireParamFor(t, old, imports)
		if err != nil {
			return "", "", nil, err
		}
		imports = withParam
		param, paramType, what, depsType := p.name, p.typ, p.what, p.depsType
		field, pass, why := p.field, p.pass, p.why
		switch {
		case t.declared:
			if !t.idx.wanted[ctor] {
				t.idx.wanted[ctor] = true
				where := t.dir
				if rel, err := filepath.Rel(b.dir, t.dir); err == nil {
					where = rel
				}
				b.notef("wire: %s has no %s, which the generated Wire calls, so the build fails until there is one. Write func %s(%s %s) *%s in %s.",
					t.target, ctor, ctor, param, paramType, t.typ, where)
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
				case b.wire.structRead && !b.wireShared(t.target) && b.wireAnyField():
					b.notef("wire: the %s group has no field named after it in %s, so %s takes the whole value and its package imports everything that names. Add the field and change the constructor to keep its builds apart.",
						t.name, depsType, ctor)
				}
			}
			fields = " {\n\t" + keep + " " + paramType + "\n}"
			constructor = fmt.Sprintf("\n// %s builds the %s group's Resolver from %s.\n// The generated Wire calls it.\nfunc %s(%s %s) *%s { return &%s{%s: %s} }\n",
				ctor, t.name, what, ctor, param, paramType, t.typ, t.typ, keep, param)
			t.idx.funcs[ctor] = true
		}
	}
	return fields, constructor, imports, nil
}

// wireParam is what a scaffolded constructor takes: the field named after the
// group, or the whole dependencies value. See wireArg.
type wireParam struct {
	name, typ string
	// what describes the parameter in the constructor's doc comment.
	what     string
	depsType string
	field    *depsField
	pass     *passed
	// why is the reason a field could not be handed over, when one exists.
	why error
}

// wireParamFor resolves the constructor parameter for t, and the imports
// writing its type adds to the file.
func (b *builder) wireParamFor(t *scaffoldTarget, old []byte, imports []importSpec) (*wireParam, []importSpec, error) {
	depsPath, depsQual, depsType := b.wireDeps()
	implImport, _ := b.importPathOf(t.dir)
	// Each package the constructor's parameter names is imported once,
	// under a name free in this file.
	chosen := map[string]string{}
	var qualifyErr error
	qualify := func(importPath, preferred string) string {
		if q, ok := chosen[importPath]; ok {
			return q
		}
		q, add, err := depsQualifier(old, importPath, preferred, t.idx, imports, t.alias, t.typ)
		if err != nil {
			qualifyErr = err
		}
		if add {
			imports = append(imports, importSpec{q, importPath})
		}
		chosen[importPath] = q
		return q
	}
	if t.declared {
		// The note names the type as the dependencies file writes it;
		// nothing is imported for a constructor that is not written.
		qualify = func(_, preferred string) string { return preferred }
	}
	// What the constructor takes: the field named after the group, or
	// the whole value. See wireArg.
	p := &wireParam{name: "d", typ: depsType, what: "the application's dependencies", depsType: depsType}
	// Wire.Pass decides where it speaks, and has been checked by the
	// time anything is scaffolded; otherwise the struct does.
	pass, err := b.wirePass(t.name, t.target)
	if err != nil {
		return nil, nil, err
	}
	p.pass = pass
	if pass != nil {
		p.field = pass.field
	} else {
		p.field, p.why = b.wireArg(t.name, t.target)
	}
	switch {
	case p.field != nil:
		placed, err := p.field.placed(depsPath, depsQual, implImport, qualify)
		if err != nil {
			return nil, nil, fmt.Errorf("wire: %s.%s: %w", depsType, p.field.name, err)
		}
		p.name, p.typ, p.what = scaffoldParam, placed, "the "+p.field.name+" field of the application's dependencies"
	case implImport != depsPath:
		p.typ = qualify(depsPath, depsQual) + "." + depsType
	}
	if pass != nil && pass.pointer {
		p.typ, p.what = "*"+p.typ, "a pointer to "+p.what
	}
	if qualifyErr != nil {
		return nil, nil, fmt.Errorf("%s: %w", t.file, qualifyErr)
	}
	return p, imports, nil
}

// writeScaffold adds body to the target's resolvers file, creating it when
// there is none, with the imports body needs.
func (b *builder) writeScaffold(t *scaffoldTarget, old []byte, body string, imports []importSpec) error {
	src := append(append([]byte{}, old...), body...)
	if !t.self && bytes.Contains(src, []byte(t.alias+".")) {
		imports = append([]importSpec{{t.alias, t.groupImport}}, imports...)
	}
	var out []byte
	var err error
	if len(old) == 0 {
		out, err = newResolversFile(t.pkg, body, imports, b.packageName, t.idx.names)
	} else {
		out, err = addImports(src, len(old), imports, b.packageName, t.idx.names)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", t.file, err)
	}
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return err
	}
	// Only a file with something in it is the author's to lose.
	if len(old) > 0 {
		return replaceFile(t.file, out)
	}
	return os.WriteFile(t.file, out, 0o644)
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
