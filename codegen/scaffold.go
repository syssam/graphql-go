package codegen

import (
	"bytes"
	"fmt"
	goast "go/ast"
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
// the compiler's to report, through the var _ Resolver assertion.

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
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		group := name
		if flat {
			group = ""
		}
		if !known[name] || !b.hasResolver(group) {
			return fmt.Errorf("codegen: scaffold: %q is not a group with a Resolver (groups: %s)",
				name, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
		if err := b.scaffoldGroup(name, group, b.cfg.Scaffold[name]); err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
	}
	return nil
}

// scaffoldGroup brings one implementation up to its group's interface.
// target is "dir.Type", dir relative to Config.Dir.
func (b *builder) scaffoldGroup(name, group, target string) error {
	i := strings.LastIndex(target, ".")
	if i <= 0 || i == len(target)-1 {
		return fmt.Errorf("target %q is not dir.Type", target)
	}
	relDir, typ := target[:i], target[i+1:]
	dir := filepath.Join(b.dir, filepath.FromSlash(relDir))
	have, declared, pkg, err := existingMethods(dir, typ)
	if err != nil {
		return err
	}
	if pkg == "" {
		pkg = identFrom(filepath.Base(dir))
	}

	groupImport := b.cfg.Package
	if name != b.pkgName || group != "" {
		groupImport += "/" + group
	}
	alias := identFrom(name) + "gql"
	var stubs strings.Builder
	for _, m := range b.resolverSignatures(group, alias+".") {
		if have[m.name] {
			continue
		}
		stubs.WriteString("\n")
		if m.doc != "" {
			for _, line := range strings.Split(strings.TrimSpace(m.doc), "\n") {
				stubs.WriteString("// " + line + "\n")
			}
			stubs.WriteString("//\n")
		}
		fmt.Fprintf(&stubs, "// %s resolves %s.\n", m.name, m.coord)
		fmt.Fprintf(&stubs, "func (r *%s) %s%s {\n\tpanic(%q)\n}\n", typ, m.name, m.signature, "not implemented: "+m.coord)
	}
	if stubs.Len() == 0 && declared {
		return nil
	}
	body := stubs.String()
	if !declared {
		body = fmt.Sprintf("\n// %s implements the %s group's Resolver.\ntype %s struct{}\n\nvar _ %s.Resolver = (*%s)(nil)\n",
			typ, name, typ, alias, typ) + body
	}

	file := filepath.Join(dir, identFrom(name)+".resolvers.go")
	old, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	src := append(append([]byte{}, old...), body...)
	imports, err := b.stubImports(body, alias, groupImport, bytes.Contains(src, []byte(alias+".")))
	if err != nil {
		return err
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
		return fmt.Errorf("%s: %w", file, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o644)
}

// existingMethods lists the methods declared on typ in dir's non-test files,
// whether typ itself is declared there, and the package name. A directory
// that does not exist yet has none of the three.
func existingMethods(dir, typ string) (map[string]bool, bool, string, error) {
	have := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return have, false, "", nil
	}
	if err != nil {
		return nil, false, "", err
	}
	declared := false
	pkg := ""
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, false, "", err
		}
		pkg = f.Name.Name
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *goast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 && receiverName(d.Recv.List[0].Type) == typ {
					have[d.Name.Name] = true
				}
			case *goast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*goast.TypeSpec); ok && ts.Name.Name == typ {
						declared = true
					}
				}
			}
		}
	}
	return have, declared, pkg, nil
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
