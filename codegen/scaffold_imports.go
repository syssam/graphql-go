package codegen

import (
	"bytes"
	"fmt"
	goast "go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

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
