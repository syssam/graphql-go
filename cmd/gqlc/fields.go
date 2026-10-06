package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/syssam/graphql-go/codegen"
)

// fieldEntry is one field under the fields key: how that field is answered,
// said outright where gqlc would otherwise work it out.
type fieldEntry struct {
	// resolver puts the field on its group's Resolver interface whatever Go
	// field or method could answer it. It is for a field a service gates: a
	// salary bound to the struct is read past the gate by every connection,
	// node(id) and edge that reaches it.
	Resolver bool `yaml:"resolver"`
	// name is the Go struct field that answers, where it is not the one gqlc
	// derives: url is Url, and a struct gqlc did not write may say URL.
	Name string `yaml:"name"`
	// method makes name a method of the model instead.
	Method bool `yaml:"method"`
	// value says the struct field holds a value where the SDL position is
	// nullable, so the binding returns T and not *T.
	Value bool `yaml:"value"`
}

// binding is the entry as the generator takes it. An entry that could be
// read two ways is refused: on a list that exists so a gated field is not
// bound to its struct, picking one reading is a silent override.
func (e fieldEntry) binding() (codegen.FieldBinding, error) {
	switch {
	case e.Resolver:
		var with []string
		if e.Name != "" {
			with = append(with, "name")
		}
		if e.Method {
			with = append(with, "method")
		}
		if e.Value {
			with = append(with, "value")
		}
		if len(with) > 0 {
			return codegen.FieldBinding{}, fmt.Errorf("resolver: true stands alone, and %s is set too; a field is a Resolver method or bound to Go, not both", strings.Join(with, ", "))
		}
		return codegen.FieldBinding{Kind: codegen.FieldResolver}, nil
	case e.Method && e.Value:
		return codegen.FieldBinding{}, errors.New("method and value are both set; value is for a struct field")
	case e.Name == "" && e.Method:
		return codegen.FieldBinding{}, errors.New("method: true needs name, the Go method to call")
	case e.Name == "" && e.Value:
		return codegen.FieldBinding{}, errors.New("value: true needs name, the Go field holding the value")
	case e.Name == "":
		return codegen.FieldBinding{}, errors.New("the entry says nothing; write resolver: true, or name")
	case !token.IsIdentifier(e.Name) || !token.IsExported(e.Name):
		return codegen.FieldBinding{}, fmt.Errorf("name %q is not an exported Go identifier", e.Name)
	case e.Method:
		return codegen.FieldBinding{Kind: codegen.FieldMethod, GoName: e.Name}, nil
	}
	return codegen.FieldBinding{Kind: codegen.FieldStruct, GoName: e.Name, Value: e.Value}, nil
}

// manifest turns the fields key into the generator's Manifest, or nil when
// the key is absent. Each type carries no Go type of its own, which leaves its
// binding to models, the model directive or autoBind and overrides only the
// fields named. Every refused entry is reported, not the first.
func (fc *fileConfig) manifest() (*codegen.Manifest, error) {
	if len(fc.Fields) == 0 {
		return nil, nil
	}
	m := &codegen.Manifest{}
	var errs []error
	for _, typ := range slices.Sorted(maps.Keys(fc.Fields)) {
		entries := fc.Fields[typ]
		// What a mis-indented entry leaves behind: the type, and its fields
		// read as types of their own.
		if len(entries) == 0 {
			errs = append(errs, fmt.Errorf("fields: %s names no field", typ))
			continue
		}
		bound := make(map[string]codegen.FieldBinding, len(entries))
		for _, field := range slices.Sorted(maps.Keys(entries)) {
			fb, err := entries[field].binding()
			if err != nil {
				errs = append(errs, fmt.Errorf("fields: %s.%s: %w", typ, field, err))
				continue
			}
			bound[field] = fb
		}
		m.Types = append(m.Types, codegen.TypeBinding{Name: typ, Fields: bound})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// forcedResolvers lists every field marked resolver: true, as Type.field.
func (fc *fileConfig) forcedResolvers() []string {
	var out []string
	for typ, entries := range fc.Fields {
		for field, e := range entries {
			if e.Resolver {
				out = append(out, typ+"."+field)
			}
		}
	}
	slices.Sort(out)
	return out
}

// checkForcedResolvers reads the generated code back and fails naming each
// forced field that no Resolver method answers.
//
// The generator is asked for these and this checks that it obliged, because
// the failure is silent otherwise: a field bound to its struct compiles,
// serves, and skips the gate the resolver was to call. The generator refuses
// the entries it knows cannot force anything -- a field of an interface or an
// input object -- before it writes; this is the second guard on the same
// list, for whatever it accepts and then does not do.
//
// It reads rather than recomputes. Above each Resolver method the generator
// writes "// Method resolves Type.field.", so the coordinate is matched as
// written and the method's name, which fieldNames and the group both shape,
// is never derived a second time here. A method counts when it is on the
// Resolver interface under that line and something in the file calls it. If
// the generator changes the line, every forced field is reported missing,
// which is the direction to fail in.
func checkForcedResolvers(outDir, groupDir string, forced []string) error {
	if len(forced) == 0 {
		return nil
	}
	want := make(map[string]bool, len(forced))
	for _, coord := range forced {
		want[coord] = true
	}
	// One group is the output package itself; several are a package each,
	// under groupDir when that is set.
	groups := filepath.Join(outDir, filepath.FromSlash(groupDir))
	entries, err := os.ReadDir(groups)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("gqlc: fields: %w", err)
	}
	dirs := []string{outDir}
	for _, e := range entries {
		dirs = append(dirs, filepath.Join(groups, e.Name()))
	}
	found := map[string]bool{}
	for i, dir := range dirs {
		path := filepath.Join(dir, "generated.go")
		src, err := os.ReadFile(path)
		if err != nil {
			// Not a group: a file, or a directory holding something else.
			continue
		}
		// Most groups force nothing, and parsing is the cost: with this, 44 ms
		// for 63 fields over 578 groups and 7 MB of generated code.
		if !mentions(src, want) {
			continue
		}
		if i > 0 && otherRunsOutput(dir) {
			continue
		}
		if err := resolverFields(path, src, want, found); err != nil {
			return fmt.Errorf("gqlc: fields: %w", err)
		}
	}
	var missing []string
	for _, coord := range forced {
		if !found[coord] {
			missing = append(missing, coord)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("gqlc: fields: %d of %d fields marked resolver: true did not become Resolver methods, so nothing a resolver checks runs for them: %s (only an object type's field can be forced; for an interface's, name each object implementing it)",
		len(missing), len(forced), strings.Join(missing, ", "))
}

// mentions reports whether src has the generator's line for a field in want,
// in one pass and without parsing.
func mentions(src []byte, want map[string]bool) bool {
	const marker = " resolves "
	for {
		i := bytes.Index(src, []byte(marker))
		if i < 0 {
			return false
		}
		src = src[i+len(marker):]
		line, _, _ := bytes.Cut(src, []byte("\n"))
		if want[strings.TrimSuffix(string(bytes.TrimRight(line, "\r")), ".")] {
			return true
		}
	}
}

// otherRunsOutput reports whether dir is the root of another gqlc run nested
// in this one's output: a generated schema.go beside the schema directory it
// embeds, the two signs the generator itself goes by. Such a directory is
// never a group of this run, which refuses to write a group into one, so a
// Resolver there answers nothing here.
func otherRunsOutput(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "schema")); err != nil || !fi.IsDir() {
		return false
	}
	src, err := os.ReadFile(filepath.Join(dir, "schema.go"))
	return err == nil && bytes.HasPrefix(bytes.TrimPrefix(src, []byte("\ufeff")), []byte("// Code generated by gqlc. DO NOT EDIT."))
}

// resolverFields marks in found each field in want that the file's Resolver
// interface has a method for and the file calls.
func resolverFields(path string, src []byte, want, found map[string]bool) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	methods := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Resolver" {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			for _, m := range it.Methods.List {
				if len(m.Names) != 1 || m.Doc == nil {
					continue
				}
				name := m.Names[0].Name
				coord, ok := strings.CutPrefix(m.Doc.List[0].Text, "// "+name+" resolves ")
				if coord = strings.TrimSuffix(coord, "."); ok && want[coord] {
					methods[name] = coord
				}
			}
		}
	}
	if len(methods) == 0 {
		return nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if coord, ok := methods[sel.Sel.Name]; ok {
					found[coord] = true
				}
			}
		}
		return true
	})
	return nil
}
