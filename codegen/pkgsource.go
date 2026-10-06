package codegen

import (
	"bytes"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

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

// packageName is the name of the package at importPath, read from its
// package clause. An import with no name is known by that name and not by
// its directory's, and guessing the directory's resolved a type to the wrong
// package when two imports had each other's. It falls back to the guess for
// a package this run cannot read.
//
// Each ask reads every source file of the package, and Wire asks once for
// every import of every file beside a constructor, so the answer is kept --
// while nothing has been written. Scaffolding starts after the output is on
// disk and adds files as it goes, and a package this run creates answers
// differently before and after: graph/v2 is guessed to be graph until its
// first file says v2. From there each ask reads again.
func (b *builder) packageName(importPath string) string {
	if b.scaffolding {
		return b.readPackageName(importPath)
	}
	name, ok := b.pkgNames[importPath]
	if !ok {
		name = b.readPackageName(importPath)
		if b.pkgNames == nil {
			b.pkgNames = map[string]string{}
		}
		b.pkgNames[importPath] = name
	}
	return name
}

func (b *builder) readPackageName(importPath string) string {
	b.pkgNameReads++
	if dir := b.dirOf(importPath); dir != "" {
		// A directory can hold a file of package main under a build tag -- a
		// generator kept beside what it generates -- so main is the answer
		// only when nothing says otherwise.
		files, main := sourceFiles(dir), false
		for _, name := range slices.Sorted(maps.Keys(files)) {
			f, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.PackageClauseOnly)
			switch {
			case err != nil:
			case f.Name.Name == "main":
				main = true
			default:
				return f.Name.Name
			}
		}
		if main {
			return "main"
		}
	}
	return identFrom(lastElement(importPath))
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
