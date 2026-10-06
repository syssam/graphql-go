package graphql

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateAPI = flag.Bool("update-api", false, "rewrite docs/public-api.txt from the current source")

// The public API is the one thing about a library that cannot be fixed in a
// patch release. api_align_test.go covers how it behaves; nothing covered what
// it is, so adding, removing or re-signing an exported symbol was invisible in
// review unless someone happened to look.
//
// This walks every package a consumer can import and compares its exported
// surface against docs/public-api.txt. The golden file is the point: a
// deliberate API change is a visible diff in the same commit that makes it,
// and an accidental one fails here.
//
//	go test -run TestPublicAPISurface -update-api .
func TestPublicAPISurface(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	got := publicAPI(t, root)

	const golden = "docs/public-api.txt"
	if *updateAPI {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d lines)", golden, strings.Count(got, "\n"))
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v\n\nRun: go test -run TestPublicAPISurface -update-api .", err)
	}
	if got == string(want) {
		return
	}
	// Report the difference as symbols rather than as a text diff: what
	// matters is which symbol appeared or vanished, not which line moved.
	added, removed := lineDiff(strings.Split(string(want), "\n"), strings.Split(got, "\n"))
	var b strings.Builder
	b.WriteString("the public API has changed.\n")
	for _, l := range removed {
		fmt.Fprintf(&b, "  - %s\n", l)
	}
	for _, l := range added {
		fmt.Fprintf(&b, "  + %s\n", l)
	}
	b.WriteString("\nIf that is intended, run: go test -run TestPublicAPISurface -update-api .")
	t.Fatal(b.String())
}

// publicAPI renders every exported declaration of every importable package
// under root, sorted, one per line.
func publicAPI(t *testing.T, root string) string {
	t.Helper()
	fset := token.NewFileSet()
	src := &apiSource{root: root, module: modulePath(t, root), fset: fset, dirs: map[string][]*ast.File{}}
	var lines []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, path, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("%s: %w", path, perr)
		}
		// A main package is a command, not API.
		if f.Name.Name == "main" {
			return nil
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		pkg := filepath.ToSlash(rel)
		if pkg == "." {
			pkg = "graphql"
		}
		lines = append(lines, src.declLines(pkg, filepath.Dir(path), f)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	slices.Sort(lines)
	lines = slices.Compact(lines)
	return strings.Join(lines, "\n") + "\n"
}

// skipDir keeps the walk to what a consumer can import: no internal packages,
// no examples, no vendored reference sources, and no sibling modules, which
// have their own go.mod and are not part of this one.
func skipDir(root, path, name string) bool {
	if path == root {
		return false
	}
	switch name {
	case "internal", "examples", "ref", "testdata", "scripts", "docs", ".git", ".github":
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	// A directory with its own go.mod is a separate module.
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
		return true
	}
	return false
}

// apiSource is what rendering a declaration needs beyond the declaration: the
// module it is in, so that a reference into one of its own packages can be
// followed to what it refers to.
type apiSource struct {
	root, module string
	fset         *token.FileSet
	dirs         map[string][]*ast.File
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(mod)) {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(name)
		}
	}
	t.Fatalf("no module line in %s", filepath.Join(root, "go.mod"))
	return ""
}

func (src *apiSource) declLines(pkg, dir string, f *ast.File) []string {
	var out []string
	emit := func(s string) {
		out = append(out, pkg+": "+strings.Join(strings.Fields(s), " "))
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !isAPI(d) {
				continue
			}
			// Signature only: a body is an implementation detail and would
			// make every refactor a diff here.
			sig := *d
			sig.Body = nil
			sig.Doc = nil
			emit(renderNode(src.fset, &sig))
		case *ast.GenDecl:
			for i, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					emit("type " + renderNode(src.fset, exportedOnly(s)))
				case *ast.ValueSpec:
					for n, name := range s.Names {
						if !name.IsExported() {
							continue
						}
						// The value is appended whole: emit folds runs of
						// white space, and inside a constant that is content.
						out = append(out, pkg+": "+d.Tok.String()+" "+name.Name+src.valueText(dir, f, d, i, n, 0))
					}
				}
			}
		}
	}
	return out
}

// valueText renders what a variable or a constant is beyond its name: the
// type it declares, and what it is set to. The name alone let the type of an
// exported variable change, and the value of an exported constant, without a
// line here moving.
//
// A constant's value is always shown; it is compiled into every caller. A
// variable's is shown only when no type is declared, because then the
// initializer is the only statement of the type there is. This reads source,
// not types, so the initializer stands for the type rather than naming it,
// and when it is a reference into this module -- which is how the transports
// re-export a default kept in an internal package -- the reference is
// followed, since otherwise the type could change behind an unchanged line.
func (src *apiSource) valueText(dir string, f *ast.File, d *ast.GenDecl, spec, name, depth int) string {
	s := d.Specs[spec].(*ast.ValueSpec)
	typ, values := s.Type, s.Values
	// A constant with neither repeats the last one that had them, with iota
	// moved on: that is how `B` after `A Kind = iota` gets its type and value.
	for i := spec; d.Tok == token.CONST && typ == nil && len(values) == 0 && i > 0; {
		i--
		prev := d.Specs[i].(*ast.ValueSpec)
		typ, values = prev.Type, prev.Values
	}

	var b strings.Builder
	if typ != nil {
		b.WriteString(" " + oneLine(renderNode(src.fset, typ)))
	}
	if len(values) == 0 || (d.Tok == token.VAR && typ != nil) {
		return b.String()
	}
	value := values[0]
	if name < len(values) {
		value = values[name]
	}
	b.WriteString(" = " + src.renderValue(value))
	if usesIota(value) {
		b.WriteString(" (iota " + strconv.Itoa(spec) + ")")
	}
	if depth < 4 {
		b.WriteString(src.follow(dir, f, value, depth))
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// renderValue prints a string literal exactly, quoted onto one line, so that
// white space inside it is part of what is pinned.
func (src *apiSource) renderValue(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if v, err := strconv.Unquote(lit.Value); err == nil {
			return strconv.Quote(v)
		}
	}
	return oneLine(renderNode(src.fset, e))
}

func usesIota(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "iota" {
			found = true
		}
		return !found
	})
	return found
}

// follow renders the declaration a value refers to, when the value is nothing
// but a name declared in this module: one in the same package, or pkg.Name
// for a package the file imports from this module.
func (src *apiSource) follow(dir string, f *ast.File, e ast.Expr, depth int) string {
	target, name := dir, ""
	switch e := e.(type) {
	case *ast.Ident:
		name = e.Name
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return ""
		}
		target, name = src.importedDir(f, pkg.Name), e.Sel.Name
	}
	if target == "" || name == "" {
		return ""
	}
	for _, file := range src.parseDir(target) {
		for _, decl := range file.Decls {
			d, ok := decl.(*ast.GenDecl)
			if !ok || (d.Tok != token.VAR && d.Tok != token.CONST) {
				continue
			}
			for i, spec := range d.Specs {
				for n, id := range spec.(*ast.ValueSpec).Names {
					if id.Name == name {
						return " -> " + d.Tok.String() + " " + name + src.valueText(target, file, d, i, n, depth+1)
					}
				}
			}
		}
	}
	return ""
}

// importedDir is the directory of the package f imports under name, when
// that package is in this module.
func (src *apiSource) importedDir(f *ast.File, name string) string {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		rel, inModule := strings.CutPrefix(path, src.module+"/")
		if !inModule {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == name {
			return filepath.Join(src.root, filepath.FromSlash(rel))
		}
	}
	return ""
}

func (src *apiSource) parseDir(dir string) []*ast.File {
	if files, ok := src.dirs[dir]; ok {
		return files
	}
	var files []*ast.File
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if f, err := parser.ParseFile(src.fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution); err == nil {
			files = append(files, f)
		}
	}
	src.dirs[dir] = files
	return files
}

// isAPI reports whether a declaration is reachable from outside the package.
// A method needs both halves: an exported name on an exported receiver type,
// since neither alone can be called by a consumer.
func isAPI(d *ast.FuncDecl) bool {
	if !d.Name.IsExported() {
		return false
	}
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return true
	}
	return exportedTypeName(d.Recv.List[0].Type)
}

func exportedTypeName(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.IsExported()
	case *ast.StarExpr:
		return exportedTypeName(t.X)
	case *ast.IndexExpr: // generic receiver, e.g. Loader[K, V]
		return exportedTypeName(t.X)
	case *ast.IndexListExpr:
		return exportedTypeName(t.X)
	default:
		return false
	}
}

// exportedOnly strips unexported struct fields, which are not API and would
// otherwise make every internal layout change a diff here. The size-pinned
// structs have TestStructSizes for that.
func exportedOnly(s *ast.TypeSpec) *ast.TypeSpec {
	st, ok := s.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return s
	}
	kept := make([]*ast.Field, 0, len(st.Fields.List))
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 { // embedded
			kept = append(kept, f)
			continue
		}
		var names []*ast.Ident
		for _, n := range f.Names {
			if n.IsExported() {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			kept = append(kept, &ast.Field{Names: names, Type: f.Type, Tag: f.Tag})
		}
	}
	clone := *s
	cloneStruct := *st
	cloneStruct.Fields = &ast.FieldList{List: kept}
	clone.Type = &cloneStruct
	clone.Doc, clone.Comment = nil, nil
	return &clone
}

func renderNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	return buf.String()
}

func lineDiff(want, got []string) (added, removed []string) {
	for _, l := range got {
		if l != "" && !slices.Contains(want, l) {
			added = append(added, l)
		}
	}
	for _, l := range want {
		if l != "" && !slices.Contains(got, l) {
			removed = append(removed, l)
		}
	}
	return added, removed
}
