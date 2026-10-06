package codegen

import (
	"errors"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

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
			case !mayImport(self, importPath):
				bad = fmt.Errorf("its type is from %s, an internal package the group's package may not import", importPath)
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
	b.wire.fields, b.wire.targets, b.wire.ctors = map[string]*depsField{}, map[string]int{}, map[string]map[string]*constructor{}
	b.wire.dirs, b.wire.named = map[string]string{}, map[string]*depsField{}
	// What wire.go imports whatever the targets are -- the generated root,
	// each group's package, the dependencies package -- goes in first, under
	// the spelling generated code already uses, so a target is compared with
	// those as well as with other targets.
	out := outputDir(b.dir, b.cfg.Output)
	seeds := []string{out}
	if d := b.dirOf(b.cfg.Wire.Deps[:strings.LastIndex(b.cfg.Wire.Deps, ".")]); d != "" {
		seeds = append(seeds, d)
	}
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		if name != b.pkgName {
			seeds = append(seeds, filepath.Join(out, filepath.FromSlash(b.groupRel(name))))
		}
	}
	for _, d := range seeds {
		if _, seen := b.wire.dirs[strings.ToLower(d)]; !seen {
			b.wire.dirs[strings.ToLower(d)] = d
		}
	}
	// In name order, so that which spelling of a directory is met first does
	// not change from run to run.
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		if dir, typ, err := b.splitTarget(b.cfg.Scaffold[name]); err == nil {
			if dir, err = b.wireDir(dir); err == nil {
				b.wire.targets[dir+"\x00"+typ]++
			}
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
		generic  bool
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
				ts, ok := spec.(*goast.TypeSpec)
				if !ok || ts.Name.Name != depsType {
					continue
				}
				if ts.TypeParams != nil {
					generic = true
					continue
				}
				count++
				declared, _ = ts.Type.(*goast.StructType)
				in = file
			}
		}
	}
	// Generic only if nothing plain declares it: a generic declaration under
	// a build constraint beside a plain one is an alternative, and this run
	// evaluates no constraint.
	b.wire.generic = generic && count == 0
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
	b.wire.structRead = true
	for _, field := range declared.Fields.List {
		// An embedded field is a field, named after its type, and brings the
		// fields of that type with it, which nothing here can list.
		if len(field.Names) == 0 {
			b.wire.embeds = true
			t := field.Type
			if star, ok := t.(*goast.StarExpr); ok {
				t = star.X
			}
			switch g := t.(type) {
			case *goast.IndexExpr:
				t = g.X
			case *goast.IndexListExpr:
				t = g.X
			}
			if sel, ok := t.(*goast.SelectorExpr); ok {
				t = sel.Sel
			}
			if id, ok := t.(*goast.Ident); ok {
				b.wire.named[id.Name] = &depsField{name: id.Name, typ: types.ExprString(field.Type), imports: imports, dot: dot}
			}
			continue
		}
		for _, name := range field.Names {
			f := &depsField{name: name.Name, typ: types.ExprString(field.Type), imports: imports, dot: dot}
			b.wire.named[f.name] = f
			if !name.IsExported() {
				continue
			}
			if other := b.wire.fields[foldName(f.name)]; other != nil {
				other.twice, f.twice = true, true
			}
			b.wire.fields[foldName(f.name)] = f
		}
	}
}

// mayImport reports whether a package at importer may import importPath,
// which it may not when that is under an internal directory of another tree.
func mayImport(importer, importPath string) bool {
	if importer == "" {
		return true
	}
	elems := strings.Split(importPath, "/")
	for i := len(elems) - 1; i >= 0; i-- {
		if elems[i] != "internal" {
			continue
		}
		parent := strings.Join(elems[:i], "/")
		return parent == "" || importer == parent || strings.HasPrefix(importer, parent+"/")
	}
	return true
}

// wireDeps is Config.Wire.Deps taken apart: the package it is imported from,
// the qualifier generated code refers to it under, and the type's name.
func (b *builder) wireDeps() (importPath, qualifier, typeName string) {
	importPath, ref := splitModelExpr(b.cfg.Wire.Deps)
	qualifier, typeName, _ = strings.Cut(ref, ".")
	return importPath, qualifier, typeName
}
