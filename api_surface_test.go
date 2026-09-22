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
		lines = append(lines, declLines(fset, pkg, f)...)
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

func declLines(fset *token.FileSet, pkg string, f *ast.File) []string {
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
			emit(renderNode(fset, &sig))
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					emit("type " + renderNode(fset, exportedOnly(s)))
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if !n.IsExported() {
							continue
						}
						emit(d.Tok.String() + " " + n.Name)
					}
				}
			}
		}
	}
	return out
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
