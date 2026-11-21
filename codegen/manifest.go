package codegen

import (
	"fmt"
	"go/token"
	"maps"
	"slices"
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

	// ExtraEnums binds an SDL enum to a *second* Go type, in addition to the
	// one in Types. The registry is keyed on (GraphQL type, reflect.Type), so
	// both bindings coexist and each position decodes into the type it holds.
	// An ORM that generates one enum type per entity package and another per
	// column produces this; a Values map is required, because there is no
	// generated model to derive constant names from.
	ExtraEnums []TypeBinding
}

// TypeBinding binds one GraphQL type to a Go type.
type TypeBinding struct {
	// Name is the GraphQL type. It must exist in the SDL.
	Name string
	// Go is the Go type backing it. A zero GoType leaves the generated
	// model in place and binds only the fields.
	Go GoType
	// Values binds an enum's SDL values to the Go constants that carry them,
	// keyed by SDL value name. Empty means the generator derives the constant
	// names, which it can only do for an enum whose Go type it also generated.
	//
	// A mapped enum is the case this exists for: the author named the constants
	// and the generator cannot guess. One real schema has
	// AccessPolicyExpectVisible where the derived name would be
	// AccessPolicyExpectationVisible, and every such guess is a compile error in
	// a file marked DO NOT EDIT.
	Values map[string]string

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
	// Value binds a Go value where the SDL position is nullable: the field is
	// emitted returning the Go type rather than a pointer to it.
	//
	// A value that is always present trivially satisfies "may be null", and the
	// engine accepts the binding -- but AutoBind refused it, so an ORM column
	// that is NOT NULL under a nullable SDL field went to the Resolver. On one
	// real schema that was 2 179 fields, 29% of everything AutoBind left behind
	// on types it had otherwise bound.
	Value bool
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

// bound reports whether the manifest states the type's Go binding, which is what
// makes an unlisted field a Resolver rather than an inferred binding. An entry
// that names no Go type (only fields to override) does not: the type's own
// binding comes from elsewhere and the fields it does not list are inferred.
func (m *manifest) bound(typeName string) bool {
	if m == nil {
		return false
	}
	tb, ok := m.types[typeName]
	return ok && !tb.Go.zero()
}

// kindPhrase is a definition kind as an error names it.
func kindPhrase(k ast.DefinitionKind) string {
	switch k {
	case ast.Interface:
		return "an interface"
	case ast.InputObject:
		return "an input object"
	case ast.Union:
		return "a union"
	case ast.Enum:
		return "an enum"
	case ast.Scalar:
		return "a scalar"
	}
	return "not an object type"
}

// newManifest validates a manifest against the schema and folds its type
// bindings into models. It returns the merged model map so an explicit
// Config.Models entry and a manifest binding cannot silently disagree.
func newManifest(src *Manifest, schema *ast.Schema, models map[string]string) (*manifest, map[string]string, error) {
	merged := map[string]string{}
	maps.Copy(merged, models)
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
		root := (schema.Query != nil && schema.Query.Name == tb.Name) ||
			(schema.Mutation != nil && schema.Mutation.Name == tb.Name) ||
			(schema.Subscription != nil && schema.Subscription.Name == tb.Name)
		fields := make(map[string]FieldBinding, len(tb.Fields))
		for _, name := range sortedKeys(tb.Fields) {
			fb := tb.Fields[name]
			fd := def.Fields.ForName(name)
			if fd == nil {
				errs = append(errs, fmt.Sprintf("manifest: field %s.%s is not defined in the schema", tb.Name, name))
				continue
			}
			// A binding is how an object's field is answered. Anywhere else
			// it binds nothing, silently: on a list of fields forced to be
			// resolvers that is a gate believed to be there.
			if def.Kind != ast.Object {
				errs = append(errs, fmt.Sprintf("manifest: field %s.%s: %s is %s, and a field binding applies to an object type's field", tb.Name, name, tb.Name, kindPhrase(def.Kind)))
				continue
			}
			// A root has no Go value to read a field or call a method on:
			// its fields are Resolver methods and nothing else.
			if root && fb.Kind != FieldResolver {
				errs = append(errs, fmt.Sprintf("manifest: field %s.%s is a root field, which is always a Resolver method and cannot be bound to a Go field or method", tb.Name, name))
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
	errs = append(errs, checkExtraEnums(src.ExtraEnums, schema)...)
	if len(errs) > 0 {
		slices.Sort(errs)
		return nil, nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return m, merged, nil
}

// checkExtraEnums holds each second Go type to what a type binding is held to.
// Unchecked, one naming no enum bound nothing and said nothing, and one missing
// a value wrote "q.: VALUE" into generated.go, which does not parse: the
// emitter writes a constant for every SDL value, and there is no generated
// model to derive a missing one from.
func checkExtraEnums(extra []TypeBinding, schema *ast.Schema) []string {
	var errs []string
	type key struct {
		name string
		typ  GoType
	}
	seen := map[key]bool{}
	for _, tb := range extra {
		at := "manifest: extra enum " + tb.Name
		if !tb.Go.zero() {
			at += " as " + tb.Go.expr()
		}
		def := schema.Types[tb.Name]
		switch {
		case tb.Name == "":
			errs = append(errs, "manifest: extra enum: a binding has no Name")
			continue
		case def == nil:
			errs = append(errs, at+": "+tb.Name+" is not defined in the schema")
			continue
		case def.Kind != ast.Enum:
			errs = append(errs, at+": "+tb.Name+" is "+kindPhrase(def.Kind)+", and an extra enum binds an enum")
			continue
		case tb.Go.zero() || tb.Go.PkgPath == "" || !token.IsIdentifier(tb.Go.Name) || !importPathOK(tb.Go.PkgPath):
			errs = append(errs, at+": Go must name a Go type in a package, as PkgPath and Name")
			continue
		}
		k := key{tb.Name, tb.Go}
		if seen[k] {
			errs = append(errs, at+": bound more than once")
			continue
		}
		seen[k] = true
		var missing, unknown, bad []string
		for _, ev := range def.EnumValues {
			c, ok := tb.Values[ev.Name]
			switch {
			case !ok:
				missing = append(missing, ev.Name)
			case !token.IsIdentifier(c):
				bad = append(bad, ev.Name)
			}
		}
		for _, v := range sortedKeys(tb.Values) {
			if def.EnumValues.ForName(v) == nil {
				unknown = append(unknown, v)
			}
		}
		if len(missing) > 0 {
			errs = append(errs, at+": Values has no constant for "+strings.Join(missing, ", ")+"; every value needs one, since no model is generated to derive it from")
		}
		if len(unknown) > 0 {
			errs = append(errs, at+": Values names "+strings.Join(unknown, ", ")+", which "+tb.Name+" does not declare")
		}
		if len(bad) > 0 {
			errs = append(errs, at+": the constant for "+strings.Join(bad, ", ")+" is not a Go identifier")
		}
	}
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// enumConstant returns the Go constant bound to one SDL enum value, and
// whether the manifest declares one. Without it the generator derives a name
// from the SDL, which is only sound for an enum whose Go type it generated.
func (m *manifest) enumConstant(typeName, value string) (string, bool) {
	if m == nil {
		return "", false
	}
	tb, ok := m.types[typeName]
	if !ok || tb.Values == nil {
		return "", false
	}
	c, ok := tb.Values[value]
	return c, ok && c != ""
}

// mergeManifests lets an explicit manifest override discovery. Discovery is a
// guess from names; a written binding is a statement, so the statement wins,
// field by field and value by value rather than whole types.
//
// Every TypeBinding field needs a decision in mergeTypeBinding, and a field
// added without one is dropped whenever discovery also produced the type --
// Values was, and the derived constant it fell back to did not exist
// (TestMergeManifestsKeepsEveryExplicitTypeBindingField).
func mergeManifests(discovered, explicit *Manifest) *Manifest {
	if discovered == nil {
		return explicit
	}
	if explicit == nil {
		return discovered
	}
	out := &Manifest{}
	byName := map[string]int{}
	for _, tb := range discovered.Types {
		byName[tb.Name] = len(out.Types)
		out.Types = append(out.Types, tb)
	}
	for _, tb := range explicit.Types {
		i, ok := byName[tb.Name]
		if !ok {
			out.Types = append(out.Types, tb)
			continue
		}
		out.Types[i] = mergeTypeBinding(out.Types[i], tb)
	}
	// Discovery is the only producer of these, and an explicit manifest may
	// add its own: an author who knows about a second Go type says so here
	// rather than editing generated code. One naming a type discovery also
	// found replaces it, because registering one Go type twice for one enum
	// is a NewSchema error.
	for _, tb := range discovered.ExtraEnums {
		if !slices.ContainsFunc(explicit.ExtraEnums, func(e TypeBinding) bool { return e.Name == tb.Name && e.Go == tb.Go }) {
			out.ExtraEnums = append(out.ExtraEnums, tb)
		}
	}
	out.ExtraEnums = append(out.ExtraEnums, explicit.ExtraEnums...)
	return out
}

// mergeTypeBinding is one type's merge: what the explicit binding sets wins,
// and a map is merged key by key, so stating one field or one value does not
// discard what discovery found for the others.
func mergeTypeBinding(discovered, explicit TypeBinding) TypeBinding {
	merged := discovered
	if !explicit.Go.zero() {
		merged.Go = explicit.Go
	}
	if explicit.Group != "" {
		merged.Group = explicit.Group
	}
	merged.Values = mergeMaps(discovered.Values, explicit.Values)
	merged.Fields = mergeMaps(discovered.Fields, explicit.Fields)
	return merged
}

// mergeMaps is base with over's entries on top, and base itself when over is
// empty.
func mergeMaps[V any](base, over map[string]V) map[string]V {
	if len(over) == 0 {
		return base
	}
	out := make(map[string]V, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

// yieldToDeclared drops the Go type a discovery found for a type that is
// already declared in Models, keeping whatever else the discovery learned
// about it.
//
// AutoBind discovers; Models and a binding directive declare. A disagreement
// between two declarations is a mistake and stays an error, but a discovery
// that disagrees with a declaration is not one -- it is the declaration doing
// its job. The two only meet once a schema carries its bindings in the SDL:
// before ModelDirective, Models was empty on this path and the discovery had
// nothing to disagree with.
func yieldToDeclared(discovered *Manifest, models map[string]string) *Manifest {
	if discovered == nil || len(models) == 0 {
		return discovered
	}
	out := &Manifest{Types: make([]TypeBinding, len(discovered.Types)), ExtraEnums: discovered.ExtraEnums}
	copy(out.Types, discovered.Types)
	for i := range out.Types {
		if _, declared := models[out.Types[i].Name]; declared {
			out.Types[i].Go = GoType{}
		}
	}
	return out
}
