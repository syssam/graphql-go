package codegen

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// splitModelExpr separates a Config.Models entry into the package it must be
// imported from and the reference to write in generated code:
//
//	"time.Time"                  -> "time",        "time.Time"
//	"example.com/x/shop.Product" -> "example.com/x/shop", "shop.Product"
//	"gopkg.in/yaml.v3.Node"      -> "gopkg.in/yaml.v3",   "yamlv3.Node"
//	"string"                     -> "",            "string"
//
// Without this the whole import path was written inline, which is not valid
// Go, so any model outside a package the generator already imports failed.
//
// The type is what follows the last dot. Cutting at the first one after the
// last slash took yaml.v3 apart, and a path element may carry a dot. The
// reference's qualifier is the path's last element where that is an
// identifier, which is all it was before, and an identifier made from it
// where it is not.
func splitModelExpr(expr string) (importPath, ref string) {
	bare := strings.TrimLeft(expr, "*[]")
	prefix := expr[:len(expr)-len(bare)]
	dot := strings.LastIndex(bare, ".")
	if dot < 0 || dot < strings.LastIndex(bare, "/") {
		return "", expr
	}
	importPath = bare[:dot]
	qualifier := importPath[strings.LastIndex(importPath, "/")+1:]
	if !token.IsIdentifier(qualifier) {
		qualifier = identFrom(qualifier)
	}
	return importPath, prefix + qualifier + bare[dot:]
}

// modelExprOK reports whether a Config.Models entry can be written into
// generated code: a Go type naming nothing outside the universe, or
// import/path.Type behind any run of * and [], where Type may be a generic
// instantiated over predeclared types. Anything else that names a package --
// a map or a generic over another package's type -- has more than one import
// in it, which an entry has no way to spell; a named type declared for it can
// be mapped instead.
func modelExprOK(expr string) bool {
	importPath, ref := splitModelExpr(expr)
	if importPath == "" {
		// A slash is in no Go type, so this is a path with no type after it.
		_, err := parser.ParseExpr(ref)
		return err == nil && !strings.Contains(expr, "/")
	}
	if !importPathOK(importPath) {
		return false
	}
	// The type is after the last dot, so type arguments here hold no dot and
	// name no second package.
	name := ref[strings.LastIndex(ref, ".")+1:]
	if token.IsIdentifier(name) {
		return true
	}
	open := strings.IndexByte(name, '[')
	if open <= 0 || !token.IsIdentifier(name[:open]) {
		return false
	}
	inst, err := parser.ParseExpr(name)
	if err != nil {
		return false
	}
	switch inst.(type) {
	case *goast.IndexExpr, *goast.IndexListExpr:
		return true
	}
	return false
}

// importPathOK reports whether p can be written as an import path: elements
// between slashes, each of the characters the Go specification allows one.
func importPathOK(p string) bool {
	for elem := range strings.SplitSeq(p, "/") {
		if elem == "" {
			return false
		}
	}
	return !strings.ContainsFunc(p, func(r rune) bool {
		return r == utf8.RuneError || !unicode.IsGraphic(r) || unicode.IsSpace(r) ||
			strings.ContainsRune("!\"#$%&'()*,:;<=>?[\\]^`{|}", r)
	})
}

// modelExprImports maps the package qualifier used in generated code to the
// path it comes from, for every Config.Models entry that needs an import.
//
// The qualifier is decided here, once per import path, and the import block
// always writes it as an alias. Reading it off the reference instead assumed a
// package is named after its directory and that no two mapped paths share a
// last element; an ORM breaks both at once, with its entity enums in
// .../todo and its mutation inputs in .../client/todo under package
// todoclient (TestMappedPackagesGetTheirOwnQualifier).
func (b *builder) modelExprImports() map[string]string {
	if b.exprImports != nil {
		return b.exprImports
	}
	b.exprScans++
	paths := map[string]bool{}
	for _, expr := range b.cfg.Models {
		if path, _ := splitModelExpr(expr); path != "" {
			paths[path] = true
		}
	}
	out := map[string]string{}
	b.exprQualifiers = map[string]string{}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		q := pathQualifier(path, out)
		out[q] = path
		b.exprQualifiers[path] = q
	}
	b.exprImports = out
	return out
}

// reservedQualifier reports whether no mapped package may be imported under
// q: a name every generated file may already import under, one a group's file
// declares for itself, or one that is Go's own. A package whose path ends in
// /error, or in /v, was imported as that.
func reservedQualifier(q string) bool {
	return q == "graphql" || q == "context" || bindingNames[q] || predeclared(q)
}

// pathQualifier picks an identifier for an import path that taken does not
// already hold and reservedQualifier allows: the last path element, then the
// last two joined, then a numeric suffix. A major-version element (v2) is
// skipped, because it names no package.
func pathQualifier(path string, taken map[string]string) string {
	elems := strings.Split(path, "/")
	if n := len(elems); n > 1 && isMajorVersion(elems[n-1]) {
		elems = elems[:n-1]
	}
	base := identFrom(elems[len(elems)-1])
	free := func(q string) bool {
		_, used := taken[q]
		return q != "" && !used && !reservedQualifier(q)
	}
	if free(base) {
		return base
	}
	if len(elems) > 1 {
		if q := identFrom(elems[len(elems)-2]) + base; free(q) {
			return q
		}
	}
	for i := 2; ; i++ {
		if q := base + strconv.Itoa(i); free(q) {
			return q
		}
	}
}

func isMajorVersion(elem string) bool {
	if len(elem) < 2 || elem[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(elem[1:])
	return err == nil
}

// identFrom keeps the letters and digits of a path element, lower-cased, so
// go-yaml and yaml.v3 still give a usable identifier.
func identFrom(elem string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(elem) {
		if r == '_' || unicode.IsLetter(r) || (unicode.IsDigit(r) && sb.Len() > 0) {
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 0 {
		return "pkg"
	}
	return sb.String()
}

// exprRef renders a Config.Models entry as generated code refers to it, under
// the qualifier modelExprImports chose for its package.
func (b *builder) exprRef(expr string) string {
	path, ref := splitModelExpr(expr)
	if path == "" {
		return ref
	}
	b.modelExprImports()
	bare := strings.TrimLeft(ref, "*[]")
	return ref[:len(ref)-len(bare)] + b.exprQualifiers[path] + bare[strings.LastIndex(bare, "."):]
}
