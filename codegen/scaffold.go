package codegen

import (
	"bytes"
	"fmt"
	goast "go/ast"
	"go/build/constraint"
	"go/format"
	"go/parser"
	"go/token"
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

// scaffold writes stubs for every group Config.Scaffold names. Groups the
// schema does not have, or that have no Resolver, are errors: a typo there
// would otherwise scaffold nothing and say nothing.
func (b *builder) scaffold(groups []string) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	flat := len(groups) <= 1
	known := map[string]bool{}
	for _, g := range groups {
		known[g] = true
	}
	if flat {
		known = map[string]bool{b.pkgName: true}
	}
	groupOf := func(name string) string {
		if flat {
			return ""
		}
		return name
	}
	names := slices.Sorted(maps.Keys(b.cfg.Scaffold))
	for _, name := range names {
		if !known[name] || !b.hasResolver(groupOf(name)) {
			return fmt.Errorf("codegen: scaffold: %q is not a group with a Resolver (groups: %s)",
				name, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
	}
	pkgs := &pkgCache{byFold: map[string][]*pkgIndex{}}
	methods := make(map[string][]string, len(names))
	for _, name := range names {
		ms, err := b.scaffoldGroup(pkgs, name, groupOf(name), b.cfg.Scaffold[name])
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
	if stubs.Len() == 0 && declared {
		return names, nil
	}
	body := stubs.String()
	if !declared {
		body = fmt.Sprintf("\n// %s implements the %s group's Resolver.\ntype %s struct{}\n\nvar _ %sResolver = (*%s)(nil)\n",
			typ, name, typ, qual, typ) + body
	}

	file := filepath.Join(dir, identFrom(name)+".resolvers.go")
	old, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	src := append(append([]byte{}, old...), body...)
	imports, err := b.stubImports(body, alias, groupImport, !self && bytes.Contains(src, []byte(alias+".")))
	if err != nil {
		return nil, err
	}
	var out []byte
	if len(old) == 0 {
		// A new file is written whole, imports grouped the way goimports
		// would: the standard library, then everything else.
		out, err = format.Source([]byte("package " + pkg + "\n\n" + importDecl(imports) + body))
	} else {
		out, err = addImports(src, imports)
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

// indexPackage parses dir's non-test files once. A directory that does not
// exist yet is an empty package.
func (b *builder) indexPackage(dir string) (*pkgIndex, error) {
	idx := &pkgIndex{dir: dir, methods: map[string]map[string]bool{}, declared: map[string]bool{}, generated: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
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
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
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
		b.scaffoldParses++
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		gen := isGeneratedSource(src)
		idx.pkg = f.Name.Name
		top := func(name string) {
			switch {
			case name == "_" || name == "init":
			case gen:
				idx.generated[name] = true
			case authored[name] == "":
				authored[name] = n
			}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *goast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 {
					recv := receiverName(d.Recv.List[0].Type)
					if idx.methods[recv] == nil {
						idx.methods[recv] = map[string]bool{}
					}
					idx.methods[recv][d.Name.Name] = true
				} else {
					top(d.Name.Name)
				}
			case *goast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *goast.TypeSpec:
						idx.declared[s.Name.Name] = true
						top(s.Name.Name)
					case *goast.ValueSpec:
						for _, id := range s.Names {
							top(id.Name)
						}
					}
				}
			}
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

// stubImports is what the stubs refer to: the generator's own import block
// for the types, which already knows each qualifier's path, plus the group
// package under alias for its args structs and Resolver, when they are used.
func (b *builder) stubImports(body, alias, groupImport string, usesGroup bool) ([]importSpec, error) {
	var out []importSpec
	if usesGroup {
		out = append(out, importSpec{alias, groupImport})
	}
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

// addImports adds each import src does not already have by path, and
// formats the result.
func addImports(src []byte, imports []importSpec) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	for _, im := range imports {
		astutil.AddNamedImport(fset, f, im.name, im.path)
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
