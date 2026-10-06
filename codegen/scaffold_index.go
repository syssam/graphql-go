package codegen

import (
	"bytes"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

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
	// funcs is every top-level function the package declares, whoever wrote
	// it: a constructor Wire calls is scaffolded only where its name is free.
	funcs map[string]bool
	// wanted is each constructor this run has already said is missing, so a
	// type serving several groups is reported once.
	wanted map[string]bool
	// names is every top-level name the package declares, variables and
	// constants included: an import cannot be given one of them.
	names map[string]bool
	// inTests is each function the package's own test files declare, and
	// the file. Read only when Wire is configured: a constructor there is
	// not in the build Wire is part of, and a second one beside it is a
	// redeclaration as soon as the tests are built.
	inTests map[string]string
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

// indexPackage reads dir's non-test files once. A directory that does not
// exist yet is an empty package.
func (b *builder) indexPackage(dir string) (*pkgIndex, error) {
	idx := &pkgIndex{dir: dir, methods: map[string]map[string]bool{}, declared: map[string]bool{}, generated: map[string]bool{}, funcs: map[string]bool{}, wanted: map[string]bool{}, names: map[string]bool{}, inTests: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
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
		if e.IsDir() || !strings.HasSuffix(n, ".go") {
			continue
		}
		if strings.HasSuffix(n, "_test.go") {
			if !b.cfg.Wire.IsZero() {
				idx.readTestFile(filepath.Join(dir, n))
			}
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
		// Taken from the check where it parsed these bytes, and taken away:
		// from here the index is what the run asks about this file.
		f := b.checked[path]
		delete(b.checked, path)
		if f == nil || !bytes.Equal(f.src, src) {
			if f, err = b.parseTarget(path, src); err != nil {
				return nil, err
			}
		}
		gen := isGeneratedSource(src)
		idx.pkg = f.pkg
		top := func(name string) {
			idx.names[name] = true
			switch {
			case name == "_" || name == "init":
			case gen:
				idx.generated[name] = true
			case authored[name] == "":
				authored[name] = n
			}
		}
		for _, m := range f.methods {
			recv, name := m[0], m[1]
			if idx.methods[recv] == nil {
				idx.methods[recv] = map[string]bool{}
			}
			idx.methods[recv][name] = true
		}
		for _, name := range f.funcs {
			idx.funcs[name] = true
			top(name)
		}
		for _, name := range f.types {
			idx.declared[name] = true
			top(name)
		}
		for _, name := range f.values {
			top(name)
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

// readTestFile records the functions a test file of the package itself
// declares; one of an external _test package is another package's.
func (p *pkgIndex) readTestFile(path string) {
	// Read as the other files are: one no build includes is not there, and a
	// constructor is a name, however it is declared.
	src, err := os.ReadFile(path)
	if err != nil || excludedEverywhere(src) {
		return
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil || strings.HasSuffix(f.Name.Name, "_test") {
		return
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *goast.FuncDecl:
			if d.Recv == nil {
				p.inTests[d.Name.Name] = filepath.Base(path)
			}
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*goast.ValueSpec); ok {
					for _, id := range vs.Names {
						p.inTests[id.Name] = filepath.Base(path)
					}
				}
			}
		}
	}
}
