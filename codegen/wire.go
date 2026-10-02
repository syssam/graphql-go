package codegen

import (
	"errors"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/ast/astutil"
)

// depsField is one exported field of the dependencies struct, as its source
// declares it.
type depsField struct {
	name string
	// typ is the field's type as written, and imports the packages the file
	// declaring it imports, by the name each is known under there.
	typ     string
	imports map[string]string
	// dot is whether that file has a dot-import, which leaves a bare name
	// with two packages it could be from.
	dot bool
	// twice is whether another field has the same name once folded.
	twice bool
}

// foldName is a name with case and punctuation taken out, which is how a
// group is matched to a field: the orderitem group is OrderItem's.
func foldName(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// placed is the field's type as a file in another package writes it, or why
// it cannot be written there. A package it names is imported under whatever
// qualify answers for its path, and a type declared beside the struct gains
// the dependencies package's own qualifier. self is the import path of the
// package being written into, whose own types need no qualifier at all.
//
// The type written has to be the field's type exactly, since Wire passes one
// where the constructor takes the other. So anything that would be a
// different type once written again is refused rather than approximated: a
// struct or interface written inline (a tag is part of the type, and an
// unexported member belongs to the package it is written in), a name another
// package cannot use, and a bare name in a file with a dot-import.
func (f *depsField) placed(depsPath, depsQual, self string, qualify func(importPath, preferred string) string) (string, error) {
	if f.twice {
		return "", errors.New("another field has the same name once case and punctuation are taken out")
	}
	expr, err := parser.ParseExpr(f.typ)
	if err != nil {
		return "", err
	}
	var bad error
	out := astutil.Apply(expr, func(c *astutil.Cursor) bool {
		switch n := c.Node().(type) {
		case *goast.StructType:
			if n.Fields != nil && len(n.Fields.List) > 0 {
				bad = errors.New("its type is a struct written inline, which written again elsewhere is another type; give it a name")
			}
			return false
		case *goast.InterfaceType:
			if n.Methods != nil && len(n.Methods.List) > 0 {
				bad = errors.New("its type is an interface written inline; give it a name")
			}
			return false
		case *goast.SelectorExpr:
			x, ok := n.X.(*goast.Ident)
			if !ok {
				return true
			}
			importPath, ok := f.imports[x.Name]
			switch {
			case !ok:
				bad = fmt.Errorf("its type names %s, which is not a name the file declaring it imports a package under; give that import an alias", x.Name)
			case importPath == self:
				c.Replace(n.Sel)
			default:
				x.Name = qualify(importPath, x.Name)
			}
			return false
		case *goast.Ident:
			// A parameter's name, inside a func type, is not a type; and a
			// predeclared one belongs to no package.
			if c.Name() == "Names" || types.Universe.Lookup(n.Name) != nil {
				return false
			}
			switch {
			case f.dot:
				bad = fmt.Errorf("the file declaring it has a dot-import, so %s may be that package's or its own", n.Name)
			case depsPath == self:
			case !goast.IsExported(n.Name):
				bad = fmt.Errorf("its type names %s, which no other package can", n.Name)
			default:
				c.Replace(&goast.SelectorExpr{X: goast.NewIdent(qualify(depsPath, depsQual)), Sel: goast.NewIdent(n.Name)})
			}
			return false
		}
		return true
	}, nil)
	if bad != nil {
		return "", bad
	}
	return types.ExprString(out.(goast.Expr)), nil
}

// dirOf is the directory of a package in this module, worked out from where
// Output is and what it is imported as, or "" for one that is not under the
// module's root: a package of another module is somewhere this run cannot
// find from a path, and a directory that merely sits where the arithmetic
// lands is not it.
func (b *builder) dirOf(importPath string) string {
	out := outputDir(b.dir, b.cfg.Output)
	rel, err := filepath.Rel(filepath.FromSlash(b.cfg.Package), filepath.FromSlash(importPath))
	if err != nil {
		return ""
	}
	dir := filepath.Join(out, rel)
	root := out
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return dir // no module to be outside of
		}
		root = parent
	}
	if within, err := filepath.Rel(root, dir); err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return ""
	}
	return dir
}

// sourceFiles is the Go files of dir a build could include: not tests, and
// not one a //go:build line leaves out everywhere.
func sourceFiles(dir string) map[string][]byte {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || excludedEverywhere(src) {
			continue
		}
		out[e.Name()] = src
	}
	return out
}

// packageName is the name of the package at importPath, read from its
// package clause. An import with no name is known by that name and not by
// its directory's, and guessing the directory's resolved a type to the wrong
// package when two imports had each other's. It falls back to the guess for
// a package this run cannot read.
func (b *builder) packageName(importPath string) string {
	if dir := b.dirOf(importPath); dir != "" {
		files := sourceFiles(dir)
		for _, name := range slices.Sorted(maps.Keys(files)) {
			if f, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.PackageClauseOnly); err == nil {
				return f.Name.Name
			}
		}
	}
	return identFrom(lastElement(importPath))
}

// readWire reads, once, the exported fields of the dependencies struct and
// how many groups each scaffold target implements. The struct is found by
// parsing its package's directory; nothing is loaded or type-checked, and a
// package that is not in this module simply has no fields to offer. Neither
// has a struct declared in more than one file: those are alternatives a build
// constraint chooses between, and nothing here evaluates one.
func (b *builder) readWire() {
	if b.wire.read {
		return
	}
	b.wire.read = true
	b.wire.fields, b.wire.targets, b.wire.ctors = map[string]*depsField{}, map[string]int{}, map[string]map[string]string{}
	for _, target := range b.cfg.Scaffold {
		if dir, typ, err := b.splitTarget(target); err == nil {
			b.wire.targets[dir+"\x00"+typ]++
		}
	}
	depsPath, _, depsType := b.wireDeps()
	dir := b.dirOf(depsPath)
	if dir == "" {
		return
	}
	var (
		declared *goast.StructType
		in       *goast.File
		count    int
	)
	files := sourceFiles(dir)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		file, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		// Top-level declarations only: a type of this name inside a
		// function is not the one Wire takes.
		for _, d := range file.Decls {
			gd, ok := d.(*goast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*goast.TypeSpec); ok && ts.Name.Name == depsType && ts.TypeParams == nil {
					count++
					declared, _ = ts.Type.(*goast.StructType)
					in = file
				}
			}
		}
	}
	if count != 1 || declared == nil {
		return
	}
	imports, dot := map[string]string{}, false
	for _, spec := range in.Imports {
		importPath := strings.Trim(spec.Path.Value, "\"`")
		switch {
		case spec.Name == nil:
			imports[b.packageName(importPath)] = importPath
		case spec.Name.Name == ".":
			dot = true
		case spec.Name.Name != "_":
			imports[spec.Name.Name] = importPath
		}
	}
	for _, field := range declared.Fields.List {
		for _, name := range field.Names {
			if !name.IsExported() {
				continue
			}
			f := &depsField{name: name.Name, typ: types.ExprString(field.Type), imports: imports, dot: dot}
			if other := b.wire.fields[foldName(f.name)]; other != nil {
				other.twice, f.twice = true, true
			}
			b.wire.fields[foldName(f.name)] = f
		}
	}
}

// lastElement is an import path's last element, or the one before a major
// version suffix: what a package is usually named.
func lastElement(importPath string) string {
	elems := strings.Split(importPath, "/")
	last := elems[len(elems)-1]
	if isMajorVersion(last) && len(elems) > 1 {
		last = elems[len(elems)-2]
	}
	return last
}

// wireShared reports whether target implements more than one group.
func (b *builder) wireShared(target string) bool {
	b.readWire()
	dir, typ, err := b.splitTarget(target)
	return err == nil && b.wire.targets[dir+"\x00"+typ] > 1
}

// wireArg is the field of the dependencies a new constructor for a group is
// given, or nil when it is given the whole value. The error says why a field
// that is there is not handed over.
//
// A constructor taking the whole struct makes its package import everything
// the struct names. With a service per group that is every service, so every
// group's package recompiles when any one of them changes: at 300 groups, a
// method added to one service rebuilt 305 packages and took 70 s of CPU,
// against 6 packages when each group imports its own service alone. So a
// field named after the group is what the group is given. A group with no
// such field gets the whole value, and so does a type implementing several
// groups, whose one constructor cannot take a field for each.
func (b *builder) wireArg(name, target string) (*depsField, error) {
	b.readWire()
	if b.wireShared(target) {
		return nil, nil
	}
	// A target that is not dir.Type is refused by whoever reads it first.
	dir, _, err := b.splitTarget(target)
	if err != nil {
		return nil, err
	}
	f := b.wire.fields[foldName(name)]
	if f == nil {
		return nil, nil
	}
	depsPath, depsQual, depsType := b.wireDeps()
	self, _ := b.importPathOf(dir)
	if _, err := f.placed(depsPath, depsQual, self, func(_, preferred string) string { return preferred }); err != nil {
		return nil, fmt.Errorf("%s.%s is not handed to the %s group on its own: %w", depsType, f.name, name, err)
	}
	return f, nil
}

// wireAnyField reports whether any group is handed a field of its own, which
// is what makes one that is not worth a word.
func (b *builder) wireAnyField() bool {
	if b.wire.anyField == nil {
		found := false
		for name, target := range b.cfg.Scaffold {
			if f, _ := b.wireArg(name, target); f != nil {
				found = true
				break
			}
		}
		b.wire.anyField = &found
	}
	return *b.wire.anyField
}

// constructorParam is what the hand-written constructor ctor in dir takes: the
// type of its one parameter, as written. ok is false when dir has no such
// function, or it does not take exactly one thing.
//
// A constructor that exists is the author's, and it says what it wants. Wire
// goes by it before going by the struct: deciding from the struct alone broke
// every constructor written to take the whole value the moment a field named
// after its group appeared, and left a group that needs the whole value with
// a field it would have had to rename.
func (b *builder) constructorParam(dir, ctor string) (param string, ok bool) {
	b.readWire()
	found, read := b.wire.ctors[dir]
	if !read {
		found = map[string]string{}
		files := sourceFiles(dir)
		for name, src := range files {
			// Never in generated code, which is most of what is here.
			if isGeneratedSource(src) {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
			if err != nil {
				continue
			}
			for _, d := range file.Decls {
				fn, isFunc := d.(*goast.FuncDecl)
				if !isFunc || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "New") {
					continue
				}
				if ps := fn.Type.Params.List; len(ps) == 1 && len(ps[0].Names) <= 1 {
					found[fn.Name.Name] = types.ExprString(ps[0].Type)
				}
			}
		}
		b.wire.ctors[dir] = found
	}
	param, ok = found[ctor]
	return param, ok
}

// wireDeps is Config.Wire.Deps taken apart: the package it is imported from,
// the qualifier generated code refers to it under, and the type's name.
func (b *builder) wireDeps() (importPath, qualifier, typeName string) {
	importPath, ref := splitModelExpr(b.cfg.Wire.Deps)
	qualifier, typeName, _ = strings.Cut(ref, ".")
	return importPath, qualifier, typeName
}

// namesType reports whether a parameter's type, as written, is the type
// called name or a pointer to it, under whatever qualifier.
func namesType(param, name string) bool {
	param = strings.TrimPrefix(param, "*")
	return param == name || strings.HasSuffix(param, "."+name)
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

	type line struct{ bindings, constructor, arg string }
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
		// What an existing constructor takes decides; for one still to be
		// scaffolded, the struct does, as the scaffold will.
		arg := "d"
		if param, exists := b.constructorParam(dir, constructorName(typ)); !exists || !namesType(param, depsType) {
			if f, _ := b.wireArg(name, target); f != nil {
				arg += "." + f.name
			}
		}
		lines = append(lines, line{bindings, constructor, arg})
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
// something else, does not compile. A group is given the field of d named
// after it, where there is one, so its package imports that and no more.
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
		fmt.Fprintf(&w, "\t\t%s(%s(%s)),\n", l.bindings, l.constructor, l.arg)
	}
	w.WriteString("\t}\n}\n")
	return w.String(), nil
}
