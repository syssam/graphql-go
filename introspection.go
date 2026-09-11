package graphql

import (
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator/core"
)

// noIntrospectionRule rejects __schema and __type selections when
// introspection is disabled. __typename remains allowed.
var noIntrospectionRule = core.Rule{
	Name: "NoIntrospection",
	RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		observers.OnField(func(_ *core.Walker, field *ast.Field) {
			if field.Name == "__schema" || field.Name == "__type" {
				addError(core.Message("GraphQL introspection is not allowed, but the query contained %s.", field.Name), core.At(field.Position))
			}
		})
	},
}

// The introspection system is implemented as ordinary bindings over the
// meta types that gqlparser injects into every schema. Every field is pure,
// so introspection never schedules goroutines.

type introTypeKind string

const (
	kindScalar      introTypeKind = "SCALAR"
	kindObject      introTypeKind = "OBJECT"
	kindInterface   introTypeKind = "INTERFACE"
	kindUnion       introTypeKind = "UNION"
	kindEnum        introTypeKind = "ENUM"
	kindInputObject introTypeKind = "INPUT_OBJECT"
	kindList        introTypeKind = "LIST"
	kindNonNull     introTypeKind = "NON_NULL"
)

var introTypeKinds = map[introTypeKind]string{
	kindScalar: "SCALAR", kindObject: "OBJECT", kindInterface: "INTERFACE", kindUnion: "UNION",
	kindEnum: "ENUM", kindInputObject: "INPUT_OBJECT", kindList: "LIST", kindNonNull: "NON_NULL",
}

var introDirectiveLocations = func() map[ast.DirectiveLocation]string {
	all := []ast.DirectiveLocation{
		ast.LocationQuery, ast.LocationMutation, ast.LocationSubscription, ast.LocationField,
		ast.LocationFragmentDefinition, ast.LocationFragmentSpread, ast.LocationInlineFragment,
		ast.LocationVariableDefinition, ast.LocationSchema, ast.LocationScalar, ast.LocationObject,
		ast.LocationFieldDefinition, ast.LocationArgumentDefinition, ast.LocationInterface,
		ast.LocationUnion, ast.LocationEnum, ast.LocationEnumValue, ast.LocationInputObject,
		ast.LocationInputFieldDefinition,
	}
	m := make(map[ast.DirectiveLocation]string, len(all))
	for _, l := range all {
		m[l] = string(l)
	}
	return m
}()

// introSchema backs __Schema.
type introSchema struct{ s *ast.Schema }

// introType backs __Type. Named types carry def; LIST and NON_NULL wrappers
// carry the wrapped type in of.
type introType struct {
	s    *ast.Schema
	kind introTypeKind
	def  *ast.Definition
	of   *ast.Type
}

// introField backs __Field.
type introField struct {
	s   *ast.Schema
	def *ast.FieldDefinition
}

// introInputValue backs __InputValue for arguments and input object fields.
type introInputValue struct {
	s            *ast.Schema
	name         string
	description  string
	typ          *ast.Type
	defaultValue *ast.Value
	directives   ast.DirectiveList
}

// introEnumValue backs __EnumValue.
type introEnumValue struct{ def *ast.EnumValueDefinition }

// introDirective backs __Directive.
type introDirective struct {
	s   *ast.Schema
	def *ast.DirectiveDefinition
}

type introTypeArgs struct{ Name string }
type includeDeprecatedArgs struct{ IncludeDeprecated *bool }

func (a includeDeprecatedArgs) include() bool {
	return a.IncludeDeprecated != nil && *a.IncludeDeprecated
}

func namedType(s *ast.Schema, def *ast.Definition) *introType {
	if def == nil {
		return nil
	}
	var kind introTypeKind
	switch def.Kind {
	case ast.Scalar:
		kind = kindScalar
	case ast.Object:
		kind = kindObject
	case ast.Interface:
		kind = kindInterface
	case ast.Union:
		kind = kindUnion
	case ast.Enum:
		kind = kindEnum
	case ast.InputObject:
		kind = kindInputObject
	}
	return &introType{s: s, kind: kind, def: def}
}

// typeRef converts an SDL type reference into the NON_NULL/LIST wrapper chain
// ending in a named type.
func typeRef(s *ast.Schema, t *ast.Type) *introType {
	switch {
	case t.NonNull:
		return &introType{s: s, kind: kindNonNull, of: &ast.Type{NamedType: t.NamedType, Elem: t.Elem}}
	case t.Elem != nil:
		return &introType{s: s, kind: kindList, of: t.Elem}
	default:
		return namedType(s, s.Types[t.NamedType])
	}
}

func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// deprecation reads @deprecated, applying the specification's default reason.
func deprecation(dirs ast.DirectiveList) (bool, *string) {
	d := dirs.ForName("deprecated")
	if d == nil {
		return false, nil
	}
	reason := "No longer supported"
	if a := d.Arguments.ForName("reason"); a != nil && a.Value != nil && a.Value.Kind != ast.NullValue {
		reason = a.Value.Raw
	}
	return true, &reason
}

func isDeprecated(dirs ast.DirectiveList) bool {
	dep, _ := deprecation(dirs)
	return dep
}

func inputValuesOf(s *ast.Schema, args ast.ArgumentDefinitionList, includeDeprecated bool) []*introInputValue {
	out := make([]*introInputValue, 0, len(args))
	for _, a := range args {
		if !includeDeprecated && isDeprecated(a.Directives) {
			continue
		}
		out = append(out, &introInputValue{s: s, name: a.Name, description: a.Description, typ: a.Type, defaultValue: a.DefaultValue, directives: a.Directives})
	}
	return out
}

func (t *introType) name() *string {
	if t.def == nil {
		return nil
	}
	return &t.def.Name
}

func (t *introType) description() *string {
	if t.def == nil {
		return nil
	}
	return optString(t.def.Description)
}

func (t *introType) specifiedByURL() *string {
	if t.kind != kindScalar {
		return nil
	}
	if d := t.def.Directives.ForName("specifiedBy"); d != nil {
		if a := d.Arguments.ForName("url"); a != nil && a.Value != nil {
			return &a.Value.Raw
		}
	}
	return nil
}

func (t *introType) fields(a includeDeprecatedArgs) []*introField {
	if t.kind != kindObject && t.kind != kindInterface {
		return nil
	}
	out := make([]*introField, 0, len(t.def.Fields))
	for _, f := range t.def.Fields {
		if strings.HasPrefix(f.Name, "__") || (!a.include() && isDeprecated(f.Directives)) {
			continue
		}
		out = append(out, &introField{s: t.s, def: f})
	}
	return out
}

func (t *introType) interfaces() []*introType {
	if t.kind != kindObject && t.kind != kindInterface {
		return nil
	}
	out := make([]*introType, 0, len(t.def.Interfaces))
	for _, name := range t.def.Interfaces {
		out = append(out, namedType(t.s, t.s.Types[name]))
	}
	return out
}

func (t *introType) possibleTypes() []*introType {
	if t.kind != kindInterface && t.kind != kindUnion {
		return nil
	}
	possible := t.s.GetPossibleTypes(t.def)
	out := make([]*introType, 0, len(possible))
	for _, pd := range possible {
		// gqlparser also records interfaces implementing this interface;
		// the specification lists object types only.
		if pd.Kind == ast.Object {
			out = append(out, namedType(t.s, pd))
		}
	}
	return out
}

func (t *introType) enumValues(a includeDeprecatedArgs) []*introEnumValue {
	if t.kind != kindEnum {
		return nil
	}
	out := make([]*introEnumValue, 0, len(t.def.EnumValues))
	for _, v := range t.def.EnumValues {
		if !a.include() && isDeprecated(v.Directives) {
			continue
		}
		out = append(out, &introEnumValue{def: v})
	}
	return out
}

func (t *introType) inputFields(a includeDeprecatedArgs) []*introInputValue {
	if t.kind != kindInputObject {
		return nil
	}
	out := make([]*introInputValue, 0, len(t.def.Fields))
	for _, f := range t.def.Fields {
		if !a.include() && isDeprecated(f.Directives) {
			continue
		}
		out = append(out, &introInputValue{s: t.s, name: f.Name, description: f.Description, typ: f.Type, defaultValue: f.DefaultValue, directives: f.Directives})
	}
	return out
}

func (t *introType) ofType() *introType {
	if t.of == nil {
		return nil
	}
	return typeRef(t.s, t.of)
}

func (t *introType) isOneOf() *bool {
	if t.kind != kindInputObject {
		return nil
	}
	v := t.def.Directives.ForName("oneOf") != nil
	return &v
}

func (s *introSchema) types() []*introType {
	names := make([]string, 0, len(s.s.Types))
	for name := range s.s.Types {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]*introType, 0, len(names))
	for _, name := range names {
		out = append(out, namedType(s.s, s.s.Types[name]))
	}
	return out
}

func (s *introSchema) directives() []*introDirective {
	names := make([]string, 0, len(s.s.Directives))
	for name := range s.s.Directives {
		// @defer is declared by gqlparser's prelude but not executed yet, so
		// it is not advertised to clients.
		if name != "defer" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	out := make([]*introDirective, 0, len(names))
	for _, name := range names {
		out = append(out, &introDirective{s: s.s, def: s.s.Directives[name]})
	}
	return out
}

func (v *introInputValue) defaultValueString() *string {
	if v.defaultValue == nil {
		return nil
	}
	str := v.defaultValue.String()
	return &str
}

// introspectionOptions returns the bindings for the introspection schema.
// NewSchema installs them after user options when introspection is enabled.
func introspectionOptions(b *schemaBuilder) []SchemaOption {
	schema := b.ast
	opts := []SchemaOption{
		Enum("__TypeKind", introTypeKinds),
		Enum("__DirectiveLocation", introDirectiveLocations),
		Args[introTypeArgs](InputField("name", func(a *introTypeArgs, v string) { a.Name = v })),
		Args[includeDeprecatedArgs](InputField("includeDeprecated", func(a *includeDeprecatedArgs, v *bool) { a.IncludeDeprecated = v })),

		Object[introSchema]("__Schema",
			Field("description", func(s *introSchema) *string { return optString(s.s.Description) }),
			Field("types", (*introSchema).types),
			Field("queryType", func(s *introSchema) *introType { return namedType(s.s, s.s.Query) }),
			Field("mutationType", func(s *introSchema) *introType { return namedType(s.s, s.s.Mutation) }),
			Field("subscriptionType", func(s *introSchema) *introType { return namedType(s.s, s.s.Subscription) }),
			Field("directives", (*introSchema).directives),
		),
		Object[introType]("__Type",
			Field("kind", func(t *introType) introTypeKind { return t.kind }),
			Field("name", (*introType).name),
			Field("description", (*introType).description),
			Field("specifiedByURL", (*introType).specifiedByURL),
			FieldArgs("fields", (*introType).fields),
			Field("interfaces", (*introType).interfaces),
			Field("possibleTypes", (*introType).possibleTypes),
			FieldArgs("enumValues", (*introType).enumValues),
			FieldArgs("inputFields", (*introType).inputFields),
			Field("ofType", (*introType).ofType),
			Field("isOneOf", (*introType).isOneOf),
		),
		Object[introField]("__Field",
			Field("name", func(f *introField) string { return f.def.Name }),
			Field("description", func(f *introField) *string { return optString(f.def.Description) }),
			FieldArgs("args", func(f *introField, a includeDeprecatedArgs) []*introInputValue {
				return inputValuesOf(f.s, f.def.Arguments, a.include())
			}),
			Field("type", func(f *introField) *introType { return typeRef(f.s, f.def.Type) }),
			Field("isDeprecated", func(f *introField) bool { return isDeprecated(f.def.Directives) }),
			Field("deprecationReason", func(f *introField) *string { _, r := deprecation(f.def.Directives); return r }),
		),
		Object[introInputValue]("__InputValue",
			Field("name", func(v *introInputValue) string { return v.name }),
			Field("description", func(v *introInputValue) *string { return optString(v.description) }),
			Field("type", func(v *introInputValue) *introType { return typeRef(v.s, v.typ) }),
			Field("defaultValue", (*introInputValue).defaultValueString),
			Field("isDeprecated", func(v *introInputValue) bool { return isDeprecated(v.directives) }),
			Field("deprecationReason", func(v *introInputValue) *string { _, r := deprecation(v.directives); return r }),
		),
		Object[introEnumValue]("__EnumValue",
			Field("name", func(v *introEnumValue) string { return v.def.Name }),
			Field("description", func(v *introEnumValue) *string { return optString(v.def.Description) }),
			Field("isDeprecated", func(v *introEnumValue) bool { return isDeprecated(v.def.Directives) }),
			Field("deprecationReason", func(v *introEnumValue) *string { _, r := deprecation(v.def.Directives); return r }),
		),
		Object[introDirective]("__Directive",
			Field("name", func(d *introDirective) string { return d.def.Name }),
			Field("description", func(d *introDirective) *string { return optString(d.def.Description) }),
			Field("isRepeatable", func(d *introDirective) bool { return d.def.IsRepeatable }),
			Field("locations", func(d *introDirective) []ast.DirectiveLocation { return d.def.Locations }),
			FieldArgs("args", func(d *introDirective, a includeDeprecatedArgs) []*introInputValue {
				return inputValuesOf(d.s, d.def.Arguments, a.include())
			}),
		),
	}

	// The root fields join the user's Query binding; when Query itself is
	// unbound, coverage validation reports that instead.
	if schema.Query != nil && b.objects[schema.Query.Name] != nil {
		opts = append(opts, Object[Root](schema.Query.Name,
			Field("__schema", func(Root) *introSchema { return &introSchema{s: schema} }),
			FieldArgs("__type", func(_ Root, a introTypeArgs) *introType { return namedType(schema, schema.Types[a.Name]) }),
		))
	}
	return opts
}
