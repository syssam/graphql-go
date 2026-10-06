package codegen

import (
	"bytes"
	"errors"
	"fmt"
	goast "go/ast"
	"go/build/constraint"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// Scaffolding writes the Resolver methods an implementation does not have
// yet, as stubs with the signature the interface asks for, so adding a field
// to the SDL is generate, then fill in a body -- not read the interface and
// copy its signature by hand.
//
// It reads the implementation's package with go/parser only: method names on
// one type, nothing type-checked and nothing loaded, which keeps the promise
// that gqlc loads no Go packages unless AutoBind asks. It never edits or
// removes a method that exists. A method whose signature no longer matches is
// the compiler's to report, through the var _ Resolver assertion; one whose
// field is gone still compiles, so reportStale names it.

// scaffoldEvery is the Scaffold key that stands for every group no other key
// names, and scaffoldGroupName is where its value has the group's name.
const (
	scaffoldEvery     = "*"
	scaffoldGroupName = "{group}"
)

// expandScaffold replaces the "*" entry with one for each group that has a
// Resolver and no entry of its own, so everything after it reads a plain map.
// It runs before anything is emitted, because Wire reads the same map.
//
// Without {group} the pattern names one type for every group. That is one
// implementation asked to be all of them, which a single group allows and
// several do not: each group's stubs go to <group>.resolvers.go beside the
// type, and Wire calls one constructor per group.
func (b *builder) expandScaffold(groups []string) error {
	pattern, ok := b.cfg.Scaffold[scaffoldEvery]
	if !ok {
		return nil
	}
	keys := b.scaffoldKeys(groups)
	if len(groups) > 1 && !strings.Contains(pattern, scaffoldGroupName) {
		return fmt.Errorf("codegen: scaffold: %q is %q, which names one type for every group; write %s where the group's name goes, as in \"graph/%s.Handler\"",
			scaffoldEvery, pattern, scaffoldGroupName, scaffoldGroupName)
	}
	// A new map: the one in Config is the caller's.
	out := make(map[string]string, len(b.cfg.Scaffold)+len(keys))
	for name, target := range b.cfg.Scaffold {
		if name != scaffoldEvery {
			out[name] = target
		}
	}
	for name, group := range keys {
		if _, listed := out[name]; listed || !b.hasResolver(group) {
			continue
		}
		out[name] = strings.ReplaceAll(pattern, scaffoldGroupName, name)
	}
	b.cfg.Scaffold = out
	return nil
}

// scaffoldKeys is the group each Scaffold key stands for. A key is its
// group's name, except where there is one group or none: the output is then a
// single package, the key is that package's name, and the group is "", which
// is what the only group is called wherever a group is asked about.
func (b *builder) scaffoldKeys(groups []string) map[string]string {
	if len(groups) <= 1 {
		return map[string]string{b.pkgName: ""}
	}
	keys := make(map[string]string, len(groups))
	for _, g := range groups {
		keys[g] = g
	}
	return keys
}

// checkScaffold refuses a key that is no group with a Resolver -- a typo
// there would otherwise scaffold nothing and say nothing -- and a target
// that is not dir.Type. Generate calls it before writing anything.
func (b *builder) checkScaffold(groups []string) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	keys := b.scaffoldKeys(groups)
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		if group, known := keys[name]; !known || !b.hasResolver(group) {
			return fmt.Errorf("codegen: scaffold: %q is not a group with a Resolver (groups: %s)",
				name, strings.Join(slices.Sorted(maps.Keys(keys)), ", "))
		}
		if _, _, err := b.splitTarget(b.cfg.Scaffold[name]); err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
	}
	return nil
}

// checkScaffoldTargets refuses, before anything is written, what scaffolding
// would refuse about each target's package once the generated code is in
// place: a directory that is not one, a file of the author's that does not
// parse, a target type the generated code declares (the gqlgen habit of
// naming the implementation Resolver), and a name of the author's that the
// generated code declares too. What the generated code declares is read from
// files, this run's output, since a gqlc file on disk that is not in it is
// about to be pruned.
func (b *builder) checkScaffoldTargets(outDir string, files map[string][]byte) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	output := b.outputPackages(outDir, files)
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		target := b.cfg.Scaffold[name]
		dir, typ, err := b.splitTarget(target)
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		entries, err := os.ReadDir(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			entries = nil
		case err != nil:
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		generated, ours := output.in(dir)
		if generated[typ] {
			return fmt.Errorf("codegen: scaffold %s: target %q: %s is declared by the generated code in that package; name the implementation something else", name, target, typ)
		}
		if seen[strings.ToLower(dir)] {
			continue
		}
		seen[strings.ToLower(dir)] = true
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			path := filepath.Join(dir, n)
			src, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("codegen: scaffold %s: %w", name, err)
			}
			// gqlc's own, and one it is about to write whatever it holds now:
			// an empty file an interrupted generate left is not the author's.
			if excludedEverywhere(src) || isGeneratedSource(src) || ours[strings.ToLower(n)] {
				continue
			}
			f, err := b.parseTarget(path, src)
			if err != nil {
				return fmt.Errorf("codegen: scaffold %s: %w", name, err)
			}
			// Kept for indexPackage, which reads this file again once the
			// output is written.
			if b.checked == nil {
				b.checked = map[string]*targetFile{}
			}
			b.checked[path] = f
			for _, declared := range f.top {
				if generated[declared] {
					return fmt.Errorf("codegen: scaffold %s: %s declares %s, which the code gqlc generates into that package declares too; rename yours",
						name, path, declared)
				}
			}
		}
	}
	return nil
}

// outputPackages is this run's Go output by the directory each file is
// written into, so that a scaffold target can be asked what the generated
// code declares beside it.
//
// A target names its directory in the author's spelling, which need not be
// the one the output is written under: another letter case where the
// filesystem ignores it, or a path through a link. So directories are matched
// as the filesystem resolves them -- but each is resolved once. Asking, for
// every target, whether every output file's directory was the target's cost
// targets times files in stats, and tripled a regenerate that had nothing to
// do at 300 groups.
type outputPackages struct {
	b      *builder
	files  map[string][]byte
	byReal map[string][]*outputPackage
}

// outputPackage is one output directory: the generated Go files in it, and,
// once asked for, the names they declare.
type outputPackage struct {
	dir       string
	rels      []string
	generated map[string]bool
	ours      map[string]bool
}

func (b *builder) outputPackages(outDir string, files map[string][]byte) *outputPackages {
	byDir := map[string]*outputPackage{}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		dir := filepath.Dir(filepath.Join(outDir, filepath.FromSlash(rel)))
		if byDir[dir] == nil {
			byDir[dir] = &outputPackage{dir: dir}
		}
		byDir[dir].rels = append(byDir[dir].rels, rel)
	}
	o := &outputPackages{b: b, files: files, byReal: map[string][]*outputPackage{}}
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		key := o.key(dir)
		o.byReal[key] = append(o.byReal[key], byDir[dir])
	}
	return o
}

// key is what two spellings of one directory share: its path with links
// resolved, lower-cased. Sharing it is not yet being one directory, which
// samePath then says.
func (o *outputPackages) key(dir string) string {
	o.b.scaffoldProbes++
	return strings.ToLower(realPath(dir))
}

// in is every name the generated code declares in dir, and the base name of
// each generated file there, lower-cased.
func (o *outputPackages) in(dir string) (generated, ours map[string]bool) {
	generated, ours = map[string]bool{}, map[string]bool{}
	for _, p := range o.byReal[o.key(dir)] {
		if filepath.Clean(p.dir) != filepath.Clean(dir) {
			o.b.scaffoldProbes++
			if !samePath(p.dir, dir) {
				continue
			}
		}
		if p.generated == nil {
			p.generated, p.ours = map[string]bool{}, map[string]bool{}
			fset := token.NewFileSet()
			for _, rel := range p.rels {
				p.ours[strings.ToLower(filepath.Base(rel))] = true
				if f, err := parser.ParseFile(fset, rel, o.files[rel], parser.SkipObjectResolution); err == nil {
					maps.Copy(p.generated, topLevelNames(f))
				}
			}
		}
		maps.Copy(generated, p.generated)
		maps.Copy(ours, p.ours)
	}
	return generated, ours
}

// topLevelNames is every name a file declares at package level, methods
// aside.
func topLevelNames(f *goast.File) map[string]bool {
	names := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *goast.FuncDecl:
			if d.Recv == nil && d.Name.Name != "init" {
				names[d.Name.Name] = true
			}
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *goast.TypeSpec:
					names[s.Name.Name] = true
				case *goast.ValueSpec:
					for _, id := range s.Names {
						if id.Name != "_" {
							names[id.Name] = true
						}
					}
				}
			}
		}
	}
	return names
}

// targetFile is what scaffolding reads of one Go file in a target's package:
// its declarations by name, which is all either reader wants of it.
type targetFile struct {
	// src is the source this was parsed from. See builder.checked.
	src []byte
	pkg string
	// top is topLevelNames, sorted.
	top []string
	// methods is each method as receiver type and name. funcs, types and
	// values are every other declaration at package level, init and the blank
	// identifier among them.
	methods              [][2]string
	funcs, types, values []string
}

// parseTarget parses one file of a target's package.
func (b *builder) parseTarget(path string, src []byte) (*targetFile, error) {
	b.scaffoldParses++
	parsed, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	f := &targetFile{src: src, pkg: parsed.Name.Name, top: slices.Sorted(maps.Keys(topLevelNames(parsed)))}
	for _, d := range parsed.Decls {
		switch d := d.(type) {
		case *goast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 {
				f.methods = append(f.methods, [2]string{receiverName(d.Recv.List[0].Type), d.Name.Name})
			} else {
				f.funcs = append(f.funcs, d.Name.Name)
			}
		case *goast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *goast.TypeSpec:
					f.types = append(f.types, s.Name.Name)
				case *goast.ValueSpec:
					for _, id := range s.Names {
						f.values = append(f.values, id.Name)
					}
				}
			}
		}
	}
	return f, nil
}

// scaffold writes stubs for every group Config.Scaffold names. Its refusals
// have been made by then, before anything was written; checkScaffold is
// asked again because scaffold is also reached on its own.
func (b *builder) scaffold(groups []string) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	if err := b.checkScaffold(groups); err != nil {
		return err
	}
	// The output is on disk by now and stubs are about to be: see packageName.
	b.scaffolding = true
	keys := b.scaffoldKeys(groups)
	names := slices.Sorted(maps.Keys(b.cfg.Scaffold))
	pkgs := &pkgCache{byFold: map[string][]*pkgIndex{}}
	methods := make(map[string][]string, len(names))
	for _, name := range names {
		ms, err := b.scaffoldGroup(pkgs, name, keys[name], b.cfg.Scaffold[name])
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		methods[name] = ms
	}

	// One type can implement several groups, so a method is stale only when
	// none of them asks for it, and it is reported once per implementation: a
	// type serving three hundred groups is one note, not three hundred. A
	// target is one type in one directory however its path is spelled --
	// "impl", "./impl", and "Impl" where the filesystem ignores case, but not
	// where it does not -- so this runs after scaffolding, when every target
	// directory exists and the filesystem can say which spellings are one.
	// pkgCache already resolved every spelling of a directory to one index, so
	// an implementation is one index and one type.
	type key struct {
		idx *pkgIndex
		typ string
	}
	type impl struct {
		spelled string
		groups  []string
		want    map[string]bool
	}
	var order []key
	impls := map[key]*impl{}
	for _, name := range names {
		dir, typ, _ := b.splitTarget(b.cfg.Scaffold[name]) // validated by scaffoldGroup
		idx, err := pkgs.get(b, dir)
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		k := key{idx, typ}
		t := impls[k]
		if t == nil {
			t = &impl{spelled: b.cfg.Scaffold[name], want: map[string]bool{}}
			impls[k] = t
			order = append(order, k)
		}
		t.groups = append(t.groups, name)
		for _, m := range methods[name] {
			t.want[m] = true
		}
	}
	for _, k := range order {
		t := impls[k]
		b.reportStale(t.groups, t.spelled, k.idx.methods[k.typ], t.want)
	}
	return nil
}

// pkgCache holds each target directory's index for one run, found the way
// implementations are: by folded path, then by samePath. Keyed by spelling,
// "Impl" and "impl" on a case-insensitive filesystem were two indexes of one
// directory, and a group reading the stale one declared a type another group
// had just written, a second time.
type pkgCache struct{ byFold map[string][]*pkgIndex }

func (c *pkgCache) get(b *builder, dir string) (*pkgIndex, error) {
	fold := strings.ToLower(dir)
	for _, idx := range c.byFold[fold] {
		if samePath(idx.dir, dir) {
			return idx, nil
		}
	}
	idx, err := b.indexPackage(dir)
	if err != nil {
		return nil, err
	}
	c.byFold[fold] = append(c.byFold[fold], idx)
	return idx, nil
}

// splitTarget reads a Scaffold target, "dir.Type" with dir relative to
// Config.Dir, into the cleaned absolute directory and the type.
func (b *builder) splitTarget(target string) (dir, typ string, err error) {
	i := strings.LastIndex(target, ".")
	if i <= 0 || i == len(target)-1 {
		return "", "", fmt.Errorf("target %q is not dir.Type", target)
	}
	return filepath.Join(b.dir, filepath.FromSlash(target[:i])), target[i+1:], nil
}

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

// reportStale names the exported methods on the implementation that its
// Resolver does not have. A field removed from the SDL leaves its method
// behind, compiling, because the var _ Resolver assertion checks only what
// the interface asks for; this run is the one that knows. It is a note, not
// an error: an exported method can be the author's own, such as String.
// want holds the methods of every Resolver the type implements -- one per
// group in groups -- since a type serving two groups has both groups' methods
// and neither group's are stale.
func (b *builder) reportStale(groups []string, target string, have, want map[string]bool) {
	var stale []string
	for m := range have {
		if !want[m] && goast.IsExported(m) {
			stale = append(stale, m)
		}
	}
	if len(stale) == 0 {
		return
	}
	slices.Sort(stale)
	which := "the " + groups[0] + " Resolver does not"
	if len(groups) > 1 {
		which = "none of the " + strings.Join(groups, ", ") + " Resolvers has"
	}
	b.notef("scaffold %s: %s has methods %s: %s. If the SDL dropped their fields, delete them.",
		strings.Join(groups, ", "), target, which, strings.Join(stale, ", "))
}

// pkgIndex is what scaffold needs of one implementation package: its name,
// the methods declared on each receiver type, and which types it declares.
type pkgIndex struct {
	dir      string
	pkg      string
	methods  map[string]map[string]bool
	declared map[string]bool
	// generated is every top-level name a gqlc-generated file in the package
	// declares, which an implementation scaffolded into that package must not
	// reuse.
	generated map[string]bool
	// funcs is every top-level function the package declares, whoever wrote
	// it: a constructor Wire calls is scaffolded only where its name is free.
	funcs map[string]bool
	// wanted is each constructor this run has already said is missing, so a
	// type serving several groups is reported once.
	wanted map[string]bool
	// names is every top-level name the package declares, variables and
	// constants included: an import cannot be given one of them.
	names map[string]bool
	// inTests is each function the package's own test files declare, and
	// the file. Read only when Wire is configured: a constructor there is
	// not in the build Wire is part of, and a second one beside it is a
	// redeclaration as soon as the tests are built.
	inTests map[string]string
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

// add records the Resolver methods of typ as declared, after its stubs and,
// if it was new, the type itself were written.
func (p *pkgIndex) add(pkg, typ string, sigs []resolverSig) {
	if p.pkg == "" {
		p.pkg = pkg
	}
	p.declared[typ] = true
	if p.methods[typ] == nil {
		p.methods[typ] = map[string]bool{}
	}
	for _, m := range sigs {
		p.methods[typ][m.name] = true
	}
}

// indexPackage reads dir's non-test files once. A directory that does not
// exist yet is an empty package.
func (b *builder) indexPackage(dir string) (*pkgIndex, error) {
	idx := &pkgIndex{dir: dir, methods: map[string]map[string]bool{}, declared: map[string]bool{}, generated: map[string]bool{}, funcs: map[string]bool{}, wanted: map[string]bool{}, names: map[string]bool{}, inTests: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	// authored is each top-level name a file gqlc did not write declares, and
	// where: in a group's own package the author's code and generated.go share
	// one namespace, and a helper named like something gqlc later emits is a
	// redeclaration the compiler reports inside a file marked DO NOT EDIT.
	//
	// Read one file after another. Reading them ioParallel at a time, as the
	// output is written, measured no faster at 300 entities (969 ms against
	// 995 ms, samples overlapping): prune has just read every one of these
	// headers in parallel, so the files are warm by the time they are parsed.
	authored := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") {
			continue
		}
		if strings.HasSuffix(n, "_test.go") {
			if !b.cfg.Wire.IsZero() {
				idx.readTestFile(filepath.Join(dir, n))
			}
			continue
		}
		path := filepath.Join(dir, n)
		// Read once: the header check and the parse share the bytes.
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if excludedEverywhere(src) {
			continue
		}
		// Taken from the check where it parsed these bytes, and taken away:
		// from here the index is what the run asks about this file.
		f := b.checked[path]
		delete(b.checked, path)
		if f == nil || !bytes.Equal(f.src, src) {
			if f, err = b.parseTarget(path, src); err != nil {
				return nil, err
			}
		}
		gen := isGeneratedSource(src)
		idx.pkg = f.pkg
		top := func(name string) {
			idx.names[name] = true
			switch {
			case name == "_" || name == "init":
			case gen:
				idx.generated[name] = true
			case authored[name] == "":
				authored[name] = n
			}
		}
		for _, m := range f.methods {
			recv, name := m[0], m[1]
			if idx.methods[recv] == nil {
				idx.methods[recv] = map[string]bool{}
			}
			idx.methods[recv][name] = true
		}
		for _, name := range f.funcs {
			idx.funcs[name] = true
			top(name)
		}
		for _, name := range f.types {
			idx.declared[name] = true
			top(name)
		}
		for _, name := range f.values {
			top(name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(authored)) {
		if idx.generated[name] {
			return nil, fmt.Errorf("%s declares %s, which the code gqlc generates into that package declares too; rename yours",
				filepath.Join(dir, authored[name]), name)
		}
	}
	return idx, nil
}

// readTestFile records the functions a test file of the package itself
// declares; one of an external _test package is another package's.
func (p *pkgIndex) readTestFile(path string) {
	// Read as the other files are: one no build includes is not there, and a
	// constructor is a name, however it is declared.
	src, err := os.ReadFile(path)
	if err != nil || excludedEverywhere(src) {
		return
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil || strings.HasSuffix(f.Name.Name, "_test") {
		return
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *goast.FuncDecl:
			if d.Recv == nil {
				p.inTests[d.Name.Name] = filepath.Base(path)
			}
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*goast.ValueSpec); ok {
					for _, id := range vs.Names {
						p.inTests[id.Name] = filepath.Base(path)
					}
				}
			}
		}
	}
}

// excludedEverywhere reports whether src's //go:build line leaves it out of
// every build: one that needs the "ignore" tag, which no platform sets and
// which by convention marks a tool file -- a "package main" beside the
// resolvers that would otherwise name a new stub file's package.
//
// A file built only elsewhere is not excluded. handler_linux.go, or a cgo
// file on a machine without a C compiler, is part of the package; judged by
// the host running gqlc, a Handler declared only there was declared again,
// and the build on the platform that has it failed.
func excludedEverywhere(src []byte) bool {
	for line := range bytes.SplitSeq(src, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("package ")) {
			return false
		}
		if !constraint.IsGoBuild(string(line)) {
			continue
		}
		expr, err := constraint.Parse(string(line))
		if err != nil {
			return false // the compiler reports it; scaffold need not guess
		}
		mentions := false
		holds := expr.Eval(func(tag string) bool {
			if tag == "ignore" {
				mentions = true
				return false
			}
			return true
		})
		return mentions && !holds
	}
	return false
}

// samePath reports whether a and b are one file or directory: by path, or --
// when both exist -- by identity, so a spelling that differs in letter case
// on a case-insensitive filesystem, or a path through a link, is still the
// same one, and on a case-sensitive filesystem two spellings stay two.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

func receiverName(e goast.Expr) string {
	if s, ok := e.(*goast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*goast.Ident); ok {
		return id.Name
	}
	return ""
}

type importSpec struct{ name, path string }

// stubImports is what the stubs' types refer to: the generator's own import
// block, which already knows each qualifier's path. The group package, for
// its args structs and Resolver, is the caller's to add.
func (b *builder) stubImports(body string) ([]importSpec, error) {
	var out []importSpec
	block := b.importBlock(body, "")
	if block == "" {
		return out, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), "", "package x\n"+block, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	for _, s := range f.Imports {
		path, _ := strconv.Unquote(s.Path.Value)
		name := ""
		if s.Name != nil {
			name = s.Name.Name
		}
		out = append(out, importSpec{name, path})
	}
	return out, nil
}

// addImports gives src, a resolvers file with stubs appended from offset
// added on, the imports those stubs need, and formats the result.
//
// The stubs name each package as the generator does, and the file is its
// author's: it may import one of them under another name, or use one of those
// names for another package. Adding the generator's import regardless
// imported a path twice, and declared a name twice wherever the file's alias
// repeated the package's own name, or its user was a service package and the
// generator's the user group's models. So the file's name for a package is
// kept and the stubs are rewritten to it, and a name the file has taken gives
// way to the next free one. local is the name an import with none is known
// by, and declared the names the package declares, which no import may have.
func addImports(src []byte, added int, imports []importSpec, local func(importPath string) string, declared map[string]bool) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	have, taken := map[string]string{}, map[string]bool{}
	for _, spec := range f.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		var name string
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			name = local(importPath)
		}
		// A dot or blank import gives the stubs no name to use.
		if name == "." || name == "_" {
			continue
		}
		taken[name] = true
		if _, dup := have[importPath]; !dup {
			have[importPath] = name
		}
	}
	rename := map[string]string{}
	for _, im := range imports {
		written := im.name
		if written == "" {
			written = local(im.path)
		}
		if name, ok := have[im.path]; ok {
			if name != written {
				rename[written] = name
			}
			continue
		}
		name := written
		for n := 2; taken[name] || declared[name]; n++ {
			name = fmt.Sprintf("%s%d", written, n)
		}
		taken[name], have[im.path] = true, name
		if name != written {
			rename[written] = name
			astutil.AddNamedImport(fset, f, name, im.path)
			continue
		}
		astutil.AddNamedImport(fset, f, im.name, im.path)
	}
	if len(rename) > 0 {
		goast.Inspect(f, func(n goast.Node) bool {
			sel, ok := n.(*goast.SelectorExpr)
			if !ok || fset.Position(sel.Pos()).Offset < added {
				return true
			}
			if x, ok := sel.X.(*goast.Ident); ok && rename[x.Name] != "" {
				x.Name = rename[x.Name]
			}
			return true
		})
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// importDecl renders imports as two groups, the standard library first.
func importDecl(imports []importSpec) string {
	var std, rest []string
	for _, im := range imports {
		line := "\t" + strconv.Quote(im.path)
		if im.name != "" {
			line = "\t" + im.name + " " + strconv.Quote(im.path)
		}
		if strings.Contains(strings.SplitN(im.path, "/", 2)[0], ".") {
			rest = append(rest, line)
		} else {
			std = append(std, line)
		}
	}
	slices.Sort(std)
	slices.Sort(rest)
	groups := []string{}
	for _, g := range [][]string{std, rest} {
		if len(g) > 0 {
			groups = append(groups, strings.Join(g, "\n"))
		}
	}
	if len(groups) == 0 {
		return ""
	}
	return "import (\n" + strings.Join(groups, "\n\n") + "\n)\n"
}
