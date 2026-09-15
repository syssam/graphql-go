package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// Manifest states bindings outright instead of letting the generator infer
// them. It is the mode an external generator wants: an ORM that already knows
// which Go type backs each GraphQL type, and which fields are struct data
// rather than resolvers, can say so and skip inference entirely.
//
// No Go type information is loaded in this mode, which is the point — loading
// the module is the cost this project exists to avoid. That is also why a
// method binding declares its own shape: the generator cannot look up whether
// a method takes a context or returns an error, so the manifest says.
type Manifest struct {
	Types []TypeBinding
}

// TypeBinding binds one GraphQL type to a Go type.
type TypeBinding struct {
	// Name is the GraphQL type. It must exist in the SDL.
	Name string
	// Go is the Go type backing it. A zero GoType leaves the generated
	// model in place and binds only the fields.
	Go GoType
	// Group overrides GroupFunc for this type. Empty means the default.
	Group string
	// Fields binds individual fields by their SDL name. A field left out is
	// a Resolver: a resolver method can always be written, where a struct
	// field that turns out not to exist is a compile error in generated code.
	Fields map[string]FieldBinding
}

// GoType names a Go type without loading it.
type GoType struct {
	// PkgPath is the import path. Empty means a predeclared type such as
	// string, or a type in the generated model package.
	PkgPath string
	// Name is the type name within that package.
	Name string
}

func (g GoType) zero() bool { return g.Name == "" }

// expr renders the type the way Config.Models spells it, so that manifest
// bindings and Models entries meet in one place rather than two.
func (g GoType) expr() string {
	if g.PkgPath == "" {
		return g.Name
	}
	return g.PkgPath + "." + g.Name
}

// FieldKind says where a field's value comes from.
type FieldKind int

const (
	// FieldResolver routes the field to the group's Resolver interface. It
	// is the zero value, so an incompletely specified manifest fails at the
	// resolver interface rather than in generated field access.
	FieldResolver FieldKind = iota
	// FieldStruct reads a struct field on the model.
	FieldStruct
	// FieldMethod calls a method on the model.
	FieldMethod
)

// FieldBinding binds one field of a type.
type FieldBinding struct {
	Kind FieldKind
	// GoName is the struct field or method name. Empty derives it from the
	// SDL name the same way the rest of the generator does.
	GoName string
	// Context makes a method take context.Context as its first parameter.
	// Ignored for other kinds.
	Context bool
	// Error makes a method return (T, error) rather than T. Ignored for
	// other kinds.
	Error bool
	// Convert wraps the value in a conversion to the Go type the field needs.
	// An ORM that stores an id as a plain string still answers an ID! field,
	// but graphql.ID is a distinct named type and Go will not assign one to
	// the other.
	Convert bool
}

// pure reports whether the binding can be read without I/O, and so be bound
// with Field rather than Resolve.
func (f FieldBinding) pure() bool {
	switch f.Kind {
	case FieldStruct:
		return true
	case FieldMethod:
		return !f.Context && !f.Error
	}
	return false
}

// goName resolves the Go identifier for a field binding.
func (f FieldBinding) goName(sdlName string) string {
	if f.GoName != "" {
		return f.GoName
	}
	return goIdent(sdlName)
}

// manifest is the builder's normalized view: lookups by name, with the type
// bindings already folded into Config.Models so that model references,
// imports and the mapped check all keep working unchanged.
type manifest struct {
	types  map[string]TypeBinding
	fields map[string]map[string]FieldBinding
	groups map[string]string
}

func (m *manifest) binding(typeName, field string) (FieldBinding, bool) {
	if m == nil {
		return FieldBinding{}, false
	}
	fs, ok := m.fields[typeName]
	if !ok {
		return FieldBinding{}, false
	}
	f, ok := fs[field]
	return f, ok
}

// bound reports whether the type is in the manifest at all, which is what
// makes an unlisted field a Resolver rather than an inferred binding.
func (m *manifest) bound(typeName string) bool {
	if m == nil {
		return false
	}
	_, ok := m.types[typeName]
	return ok
}

// newManifest validates a manifest against the schema and folds its type
// bindings into models. It returns the merged model map so an explicit
// Config.Models entry and a manifest binding cannot silently disagree.
func newManifest(src *Manifest, schema *ast.Schema, models map[string]string) (*manifest, map[string]string, error) {
	merged := map[string]string{}
	for k, v := range models {
		merged[k] = v
	}
	if src == nil {
		return nil, merged, nil
	}

	m := &manifest{
		types:  make(map[string]TypeBinding, len(src.Types)),
		fields: make(map[string]map[string]FieldBinding, len(src.Types)),
		groups: map[string]string{},
	}
	var errs []string
	for _, tb := range src.Types {
		if tb.Name == "" {
			errs = append(errs, "manifest: a type binding has no Name")
			continue
		}
		def := schema.Types[tb.Name]
		if def == nil {
			errs = append(errs, fmt.Sprintf("manifest: type %s is not defined in the schema", tb.Name))
			continue
		}
		if _, dup := m.types[tb.Name]; dup {
			errs = append(errs, fmt.Sprintf("manifest: type %s is bound more than once", tb.Name))
			continue
		}
		if !tb.Go.zero() {
			if existing, ok := merged[tb.Name]; ok && existing != tb.Go.expr() {
				errs = append(errs, fmt.Sprintf("manifest: type %s is bound to %s but Models says %s", tb.Name, tb.Go.expr(), existing))
				continue
			}
			merged[tb.Name] = tb.Go.expr()
		}
		if tb.Group != "" {
			m.groups[tb.Name] = tb.Group
		}
		m.types[tb.Name] = tb

		if len(tb.Fields) == 0 {
			continue
		}
		fields := make(map[string]FieldBinding, len(tb.Fields))
		for _, name := range sortedKeys(tb.Fields) {
			fb := tb.Fields[name]
			fd := def.Fields.ForName(name)
			if fd == nil {
				errs = append(errs, fmt.Sprintf("manifest: field %s.%s is not defined in the schema", tb.Name, name))
				continue
			}
			if fb.Kind == FieldStruct && len(fd.Arguments) > 0 {
				errs = append(errs, fmt.Sprintf("manifest: field %s.%s takes arguments and cannot be a struct field", tb.Name, name))
				continue
			}
			fields[name] = fb
		}
		m.fields[tb.Name] = fields
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return m, merged, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
