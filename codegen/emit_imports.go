package codegen

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
)

func (b *builder) importBlock(src, selfPkg string) string {
	used := usedQualifiers(src)
	var std, rest []string
	if used("context") {
		std = append(std, "\t\"context\"")
	}
	if used("graphql") {
		rest = append(rest, "\t\"github.com/syssam/graphql-go\"")
	}
	for qualifier, path := range b.modelExprImports() {
		if !used(qualifier) {
			continue
		}
		// Always aliased: modelExprImports chose the qualifier without knowing
		// the package's own name, which need not be its directory's.
		line := "\t\"" + path + "\""
		if path != qualifier {
			line = "\t" + qualifier + " \"" + path + "\""
		}
		if strings.Contains(path, ".") {
			rest = append(rest, line)
		} else {
			std = append(std, line)
		}
	}
	// An extra enum's package is named by neither Config.Models nor the
	// generated models, and its qualifier was settled in setExtraEnums, so
	// the import is written under that name whenever it differs from the
	// package's own.
	for _, qualifier := range slices.Sorted(maps.Keys(b.extraImports)) {
		if !used(qualifier) {
			continue
		}
		path := b.extraImports[qualifier]
		if base := path[strings.LastIndex(path, "/")+1:]; base == qualifier {
			rest = append(rest, "\t\""+path+"\"")
			continue
		}
		rest = append(rest, "\t"+qualifier+" \""+path+"\"")
	}
	for _, pkg := range b.modelPkgNames() {
		if pkg == selfPkg {
			continue
		}
		// The qualifier, not the package name: a generated model package whose
		// name is taken by a mapped type's package is referred to, and so
		// imported, under an alias. See modelQualifier.
		q := b.modelQualifier(pkg)
		if !used(q) {
			continue
		}
		if q == pkg {
			rest = append(rest, "\t\""+b.modelImportOf(pkg)+"\"")
			continue
		}
		rest = append(rest, "\t"+q+" \""+b.modelImportOf(pkg)+"\"")
	}
	if len(std)+len(rest) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("import (\n")
	for _, l := range std {
		out.WriteString(l + "\n")
	}
	if len(std) > 0 && len(rest) > 0 {
		out.WriteString("\n")
	}
	for _, l := range rest {
		out.WriteString(l + "\n")
	}
	out.WriteString(")\n\n")
	return out.String()
}

// usedQualifiers reports which package qualifiers a generated body really
// refers to, by parsing it rather than by searching its text.
//
// A substring search cannot tell code from a comment, and the model file
// carries "bind it with graphql.Scalar in NewSchema options" above every
// custom scalar: on one real schema that alone imported the engine into a
// thousand packages that never used it, each one a compile error in a file
// marked DO NOT EDIT. A parse also ignores a qualifier inside a string
// literal, which a search does not.
//
// The body is Go with its imports not yet prepended, so it parses. If it ever
// does not, the search stays as the fallback: an import that is present and
// unused is a better failure than one that is missing.
func usedQualifiers(src string) func(string) bool {
	body := src
	if !strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), "package ") {
		body = "package p\n" + body
	}
	f, err := parser.ParseFile(token.NewFileSet(), "", body, parser.SkipObjectResolution)
	if err != nil {
		return func(q string) bool { return strings.Contains(src, q+".") }
	}
	seen := map[string]bool{}
	goast.Inspect(f, func(n goast.Node) bool {
		if sel, ok := n.(*goast.SelectorExpr); ok {
			if id, ok := sel.X.(*goast.Ident); ok {
				seen[id.Name] = true
			}
		}
		return true
	})
	return func(q string) bool { return seen[q] }
}
