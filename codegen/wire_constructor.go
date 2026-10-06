package codegen

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ctorParam is one parameter of a hand-written constructor: its name, empty
// where it has none, and its type as written.
type ctorParam struct{ name, typ string }

// constructor is a hand-written NewX: its parameters as written, and what
// the file declaring it imports. An alias the package declares is kept in the
// same shape, since what it stands for is read through its own file's imports
// as a parameter's type is.
type constructor struct {
	// params is every parameter in order, however many there are; an alias
	// has none.
	params []ctorParam
	// param is what an alias stands for, as written; a function has none.
	param string
	// unknown is a constructor that is there and whose parameter is not
	// written where it is declared: var NewHandler = build. Nothing is
	// scaffolded over it, and nothing is inferred from it.
	unknown bool
	// imports is each imported package by the name it is known under in the
	// file; plain, the paths imported with no name of the file's own, whose
	// name is the package's and may not have been readable; dots, the paths
	// dot-imported.
	imports map[string]string
	plain   map[string]bool
	dots    []string
}

// takesType reports whether typ, the type of one of the constructor's
// parameters as its file writes it, is the dependencies type itself, and
// whether it is a pointer to it. aliases are the package's own, by name.
//
// It is read from syntax, so it is read carefully: getting it wrong passes a
// constructor that wants everything a single field, in a file marked DO NOT
// EDIT. A service that calls its own dependencies Deps is told apart by its
// package; the application's reached through a dot-import, or through an
// alias the package declares for it, is recognised.
func (c *constructor) takesType(typ, depsPath, depsType, self string, aliases map[string]*constructor) (whole, pointer bool) {
	param := strings.TrimPrefix(typ, "*")
	return c.names(param, depsPath, depsType, self, aliases, 0), param != typ
}

func (c *constructor) names(expr, depsPath, depsType, self string, aliases map[string]*constructor, depth int) bool {
	if qual, name, ok := strings.Cut(expr, "."); ok {
		if name != depsType {
			return false
		}
		if importPath, known := c.imports[qual]; known {
			return importPath == depsPath
		}
		// A qualifier no import here is known by. The file compiles, so it
		// is some unnamed import's package name, and the dependencies
		// package is imported that way: a package outside this module, say,
		// whose name is not its directory's.
		return c.plain[depsPath]
	}
	if expr == depsType && (self == depsPath || slices.Contains(c.dots, depsPath)) {
		return true
	}
	if alias := aliases["type "+expr]; alias != nil && depth < 4 {
		return alias.names(strings.TrimPrefix(alias.param, "*"), depsPath, depsType, self, aliases, depth+1)
	}
	return false
}

// existingConstructor is the hand-written constructor ctor in dir, or nil.
//
// A constructor that exists is the author's, and it says what it wants. Wire
// goes by it before going by the struct: deciding from the struct alone broke
// every constructor written to take the whole value the moment a field named
// after its group appeared, and left a group that needs the whole value with
// a field it would have had to rename.
func (b *builder) existingConstructor(dir, ctor string) *constructor {
	b.readWire()
	found, read := b.wire.ctors[dir]
	if !read {
		found = map[string]*constructor{}
		files := sourceFiles(dir)
		// In name order, so that two files a build chooses between give the
		// same answer on every run.
		for _, name := range slices.Sorted(maps.Keys(files)) {
			// Never in generated code, which is most of what is here.
			if isGeneratedSource(files[name]) {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.SkipObjectResolution)
			if err != nil {
				continue
			}
			var in *constructor
			inFile := func() *constructor {
				if in == nil {
					in = &constructor{imports: map[string]string{}, plain: map[string]bool{}}
					for _, spec := range file.Imports {
						importPath := strings.Trim(spec.Path.Value, "\"`")
						switch {
						case spec.Name == nil:
							in.imports[b.packageName(importPath)] = importPath
							in.plain[importPath] = true
						case spec.Name.Name == ".":
							in.dots = append(in.dots, importPath)
						case spec.Name.Name != "_":
							in.imports[spec.Name.Name] = importPath
						}
					}
				}
				c := *in
				return &c
			}
			add := func(name string, ft *goast.FuncType) {
				// The first that says what it takes: one that does not is
				// replaced by a later one that does, in whichever file.
				if !strings.HasPrefix(name, "New") || (found[name] != nil && !found[name].unknown) {
					return
				}
				c := inFile()
				for _, field := range ft.Params.List {
					typ := types.ExprString(field.Type)
					if len(field.Names) == 0 {
						c.params = append(c.params, ctorParam{typ: typ})
					}
					for _, id := range field.Names {
						c.params = append(c.params, ctorParam{name: id.Name, typ: typ})
					}
				}
				found[name] = c
			}
			for _, d := range file.Decls {
				switch d := d.(type) {
				case *goast.FuncDecl:
					if d.Recv == nil {
						add(d.Name.Name, d.Type)
					}
				case *goast.GenDecl:
					for _, spec := range d.Specs {
						switch spec := spec.(type) {
						case *goast.TypeSpec:
							// type Deps = deps.Deps: kept under a key no
							// function can have.
							if spec.Assign.IsValid() && spec.TypeParams == nil && found["type "+spec.Name.Name] == nil {
								c := inFile()
								c.param = types.ExprString(spec.Type)
								found["type "+spec.Name.Name] = c
							}
						case *goast.ValueSpec:
							// var NewHandler = func(...) ... is a constructor
							// too, and so is var NewHandler func(...), given
							// its value somewhere else.
							declared, _ := spec.Type.(*goast.FuncType)
							for i, id := range spec.Names {
								var lit *goast.FuncLit
								if i < len(spec.Values) {
									lit, _ = spec.Values[i].(*goast.FuncLit)
								}
								switch {
								case declared != nil:
									add(id.Name, declared)
								case lit != nil:
									add(id.Name, lit.Type)
								case strings.HasPrefix(id.Name, "New") && found[id.Name] == nil:
									// var NewHandler = build, or anything
									// else of that name: there, and what it
									// takes is not written here.
									c := inFile()
									c.unknown = true
									found[id.Name] = c
								}
							}
						}
					}
				}
			}
		}
		b.wire.ctors[dir] = found
	}
	return found[ctor]
}

// constructorName is the function Wire calls to build an implementation:
// New and the type's name, as the scaffold writes it.
func constructorName(typ string) string {
	r, size := utf8.DecodeRuneInString(typ)
	return "New" + string(unicode.ToUpper(r)) + typ[size:]
}
