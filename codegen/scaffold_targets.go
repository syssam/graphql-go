package codegen

import (
	"errors"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// checkScaffoldTargets refuses, before anything is written, what scaffolding
// would refuse about each target's package once the generated code is in
// place: a directory that is not one, a file of the author's that does not
// parse, a target type the generated code declares (the gqlgen habit of
// naming the implementation Resolver), and a name of the author's that the
// generated code declares too. What the generated code declares is read from
// files, this run's output, since a gqlc file on disk that is not in it is
// about to be pruned.
func (b *builder) checkScaffoldTargets(outDir string, files map[string][]byte) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	output := b.outputPackages(outDir, files)
	seen := b.newDirSet()
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		target := b.cfg.Scaffold[name]
		dir, typ, err := b.splitTarget(target)
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		// Asked outright: ReadDir's answer for a file where the directory
		// should be is the OS's. Windows gave one that reads as "not there
		// yet", so the refusal came after the first file was written, and a
		// check called the same setup current.
		if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
			return fmt.Errorf("codegen: scaffold %s: %s is a file, not a directory", name, dir)
		}
		entries, err := os.ReadDir(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			entries = nil
		case err != nil:
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		generated, ours := output.in(dir)
		if generated[typ] {
			return fmt.Errorf("codegen: scaffold %s: target %q: %s is declared by the generated code in that package; name the implementation something else", name, target, typ)
		}
		// Read once per directory the filesystem has, which is not once per
		// lower-cased spelling: where case matters impl and Impl are two
		// packages, and the second went unread -- its stubs unreported and a
		// generated name declared in it unrefused.
		if _, ok := seen.find(dir); ok {
			continue
		}
		seen.add(dir)
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			path := filepath.Join(dir, n)
			src, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("codegen: scaffold %s: %w", name, err)
			}
			// gqlc's own, and one it is about to write whatever it holds now:
			// an empty file an interrupted generate left is not the author's.
			if excludedEverywhere(src) || isGeneratedSource(src) || ours[strings.ToLower(n)] {
				continue
			}
			f, err := b.parseTarget(path, src)
			if err != nil {
				return fmt.Errorf("codegen: scaffold %s: %w", name, err)
			}
			// Kept for indexPackage, which reads this file again once the
			// output is written.
			if b.checked == nil {
				b.checked = map[string]*targetFile{}
			}
			b.checked[path] = f
			for _, declared := range f.top {
				if generated[declared] {
					return fmt.Errorf("codegen: scaffold %s: %s declares %s, which the code gqlc generates into that package declares too; rename yours",
						name, path, declared)
				}
			}
		}
	}
	return nil
}

// outputPackages is this run's Go output by the directory each file is
// written into, so that a scaffold target can be asked what the generated
// code declares beside it.
//
// A target names its directory in the author's spelling, which need not be
// the one the output is written under: another letter case where the
// filesystem ignores it, or a path through a link. So directories are matched
// as the filesystem resolves them -- but each is resolved once. Asking, for
// every target, whether every output file's directory was the target's cost
// targets times files in stats, and tripled a regenerate that had nothing to
// do at 300 groups.
type outputPackages struct {
	b      *builder
	files  map[string][]byte
	byReal map[string][]*outputPackage
}

// outputPackage is one output directory: the generated Go files in it, and,
// once asked for, the names they declare.
type outputPackage struct {
	dir       string
	rels      []string
	generated map[string]bool
	ours      map[string]bool
}

func (b *builder) outputPackages(outDir string, files map[string][]byte) *outputPackages {
	byDir := map[string]*outputPackage{}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		dir := filepath.Dir(filepath.Join(outDir, filepath.FromSlash(rel)))
		if byDir[dir] == nil {
			byDir[dir] = &outputPackage{dir: dir}
		}
		byDir[dir].rels = append(byDir[dir].rels, rel)
	}
	o := &outputPackages{b: b, files: files, byReal: map[string][]*outputPackage{}}
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		key := o.key(dir)
		o.byReal[key] = append(o.byReal[key], byDir[dir])
	}
	return o
}

// key is what two spellings of one directory share: its path with links
// resolved, lower-cased. Sharing it is not yet being one directory, which
// samePath then says.
func (o *outputPackages) key(dir string) string {
	o.b.scaffoldProbes++
	return strings.ToLower(realPath(dir))
}

// dirSet is a set of directories as the filesystem tells them apart:
// spellings differing in letter case are one where it ignores case and two
// where it does not, and a path through a link is the directory behind it.
// Only the filesystem knows, so samePath asks it -- but within a bucket of
// spellings that resolve and fold to one key, so a set of hundreds is not
// asked about pairwise.
type dirSet struct {
	b     *builder
	byKey map[string][]string
}

func (b *builder) newDirSet() *dirSet {
	return &dirSet{b: b, byKey: map[string][]string{}}
}

func (s *dirSet) key(dir string) string {
	s.b.scaffoldProbes++
	return strings.ToLower(realPath(dir))
}

// find is the spelling dir was added under that is dir, if any.
func (s *dirSet) find(dir string) (string, bool) {
	for _, d := range s.byKey[s.key(dir)] {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return d, true
		}
		s.b.scaffoldProbes++
		if samePath(d, dir) {
			return d, true
		}
	}
	return "", false
}

func (s *dirSet) add(dir string) {
	k := s.key(dir)
	s.byKey[k] = append(s.byKey[k], dir)
}

// in is every name the generated code declares in dir, and the base name of
// each generated file there, lower-cased.
func (o *outputPackages) in(dir string) (generated, ours map[string]bool) {
	generated, ours = map[string]bool{}, map[string]bool{}
	for _, p := range o.byReal[o.key(dir)] {
		if filepath.Clean(p.dir) != filepath.Clean(dir) {
			o.b.scaffoldProbes++
			if !samePath(p.dir, dir) {
				continue
			}
		}
		if p.generated == nil {
			p.generated, p.ours = map[string]bool{}, map[string]bool{}
			fset := token.NewFileSet()
			for _, rel := range p.rels {
				p.ours[strings.ToLower(filepath.Base(rel))] = true
				if f, err := parser.ParseFile(fset, rel, o.files[rel], parser.SkipObjectResolution); err == nil {
					maps.Copy(p.generated, topLevelNames(f))
				}
			}
		}
		maps.Copy(generated, p.generated)
		maps.Copy(ours, p.ours)
	}
	return generated, ours
}

// topLevelNames is every name a file declares at package level, methods
// aside.
func topLevelNames(f *goast.File) map[string]bool {
	names := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *goast.FuncDecl:
			if d.Recv == nil && d.Name.Name != "init" {
				names[d.Name.Name] = true
			}
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *goast.TypeSpec:
					names[s.Name.Name] = true
				case *goast.ValueSpec:
					for _, id := range s.Names {
						if id.Name != "_" {
							names[id.Name] = true
						}
					}
				}
			}
		}
	}
	return names
}

// targetFile is what scaffolding reads of one Go file in a target's package:
// its declarations by name, which is all either reader wants of it.
type targetFile struct {
	// src is the source this was parsed from. See builder.checked.
	src []byte
	pkg string
	// top is topLevelNames, sorted.
	top []string
	// methods is each method as receiver type and name. funcs, types and
	// values are every other declaration at package level, init and the blank
	// identifier among them.
	methods              [][2]string
	funcs, types, values []string
	// stubs is each method still holding the body scaffolding wrote, as
	// receiver type and the coordinate it resolves.
	stubs [][2]string
}

// stubPanic opens the message of the panic a scaffolded stub holds.
const stubPanic = "not implemented: "

// scaffoldedStub reports whether d's body is still the one scaffoldGroup
// wrote, panic("not implemented: Type.field"), and the coordinate it names.
// That exact body and nothing else: a stub the author began is theirs.
func scaffoldedStub(d *goast.FuncDecl) (string, bool) {
	if d.Body == nil || len(d.Body.List) != 1 {
		return "", false
	}
	stmt, ok := d.Body.List[0].(*goast.ExprStmt)
	if !ok {
		return "", false
	}
	call, ok := stmt.X.(*goast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	if fn, ok := call.Fun.(*goast.Ident); !ok || fn.Name != "panic" {
		return "", false
	}
	msg, ok := stringLit(call.Args[0])
	if !ok || !strings.HasPrefix(msg, stubPanic) {
		return "", false
	}
	return strings.TrimPrefix(msg, stubPanic), true
}

// parseTarget parses one file of a target's package.
func (b *builder) parseTarget(path string, src []byte) (*targetFile, error) {
	b.scaffoldParses++
	parsed, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	f := &targetFile{src: src, pkg: parsed.Name.Name, top: slices.Sorted(maps.Keys(topLevelNames(parsed)))}
	for _, d := range parsed.Decls {
		switch d := d.(type) {
		case *goast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 {
				f.methods = append(f.methods, [2]string{receiverName(d.Recv.List[0].Type), d.Name.Name})
				if coord, ok := scaffoldedStub(d); ok {
					f.stubs = append(f.stubs, [2]string{receiverName(d.Recv.List[0].Type), coord})
				}
			} else {
				f.funcs = append(f.funcs, d.Name.Name)
			}
		case *goast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *goast.TypeSpec:
					f.types = append(f.types, s.Name.Name)
				case *goast.ValueSpec:
					for _, id := range s.Names {
						f.values = append(f.values, id.Name)
					}
				}
			}
		}
	}
	return f, nil
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
