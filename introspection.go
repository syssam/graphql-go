package graphql

import (
	"slices"
	"strings"

	"github.com/syssam/graphql-go/internal/sdlprint"
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

// isIntrospectionRoot reports whether name is one of the query root's
// introspection fields. Names beginning with __ are reserved, so no schema
// declares another.
func isIntrospectionRoot(name string) bool { return name == "__schema" || name == "__type" }

// isIntrospection reports whether field name of object type obj is part of
// introspection: a root introspection field, or a field of a meta type, which
// only introspection reaches.
func isIntrospection(obj, name string) bool {
	return isIntrospectionRoot(name) || strings.HasPrefix(obj, "__")
}

// Re-entering a type's member lists is what makes introspection expensive,
// and gqlparser's MaxIntrospectionDepth rule, in its default set, is what
// refuses it. That matters more here than elsewhere, because WithMaxDepth,
// WithMaxComplexity and QueryCost do not count introspection: those limits are
// set against an API's own queries, and the introspection query every IDE and
// client generator sends is deeper than any of them and, priced by list
// sizes, costlier -- a schema of three hundred entities refused it at a depth
// limit of 10.
//
// The rule was replaced here for as long as gqlparser's walked every path
// through fragment spreads without a memo: 24 fragments each spreading the
// next twice, 1.1 KB, cost two seconds of validation. v2.5.59 memoizes it, the
// replacement is gone, and TestMaxIntrospectionDepthIsLinearInFragments stays
// to say so if that ever changes.

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

// locationDirectiveDefinition is the DIRECTIVE_DEFINITION directive location.
// gqlparser declares no constant for it because its parser cannot yet read
// one, but introspection must still offer the value.
const locationDirectiveDefinition ast.DirectiveLocation = "DIRECTIVE_DEFINITION"

var introDirectiveLocations = func() map[ast.DirectiveLocation]string {
	all := []ast.DirectiveLocation{
		ast.LocationQuery, ast.LocationMutation, ast.LocationSubscription, ast.LocationField,
		ast.LocationFragmentDefinition, ast.LocationFragmentSpread, ast.LocationInlineFragment,
		ast.LocationVariableDefinition, ast.LocationSchema, ast.LocationScalar, ast.LocationObject,
		ast.LocationFieldDefinition, ast.LocationArgumentDefinition, ast.LocationInterface,
		ast.LocationUnion, ast.LocationEnum, ast.LocationEnumValue, ast.LocationInputObject,
		ast.LocationInputFieldDefinition, locationDirectiveDefinition,
	}
	m := make(map[ast.DirectiveLocation]string, len(all))
	for _, l := range all {
		m[l] = string(l)
	}
	return m
}()

// IntrospectDeprecatedInputValues makes introspection list deprecated input fields and
// deprecated arguments when the client does not say whether it wants them.
//
// The specification hides them unless the query passes includeDeprecated: true, and graphql-js's
// getIntrospectionQuery passes nothing for inputFields and args. gqlgen returned them whatever
// was asked, so a client generated against it (a codegen that introspects the live server) lists
// every deprecated input field it still sends; against a server that follows the specification
// those fields disappear from the generated types without an error. This option changes the
// default the introspection schema declares, not the rule: a client that passes
// includeDeprecated: false still gets them hidden.
func IntrospectDeprecatedInputValues() SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		for _, site := range [][2]string{{"__Type", "inputFields"}, {"__Field", "args"}, {"__Directive", "args"}} {
			def := b.ast.Types[site[0]]
			if def == nil {
				continue
			}
			f := def.Fields.ForName(site[1])
			if f == nil {
				continue
			}
			if a := f.Arguments.ForName("includeDeprecated"); a != nil {
				a.DefaultValue = &ast.Value{Raw: "true", Kind: ast.BooleanValue}
			}
		}
	})
}

// patchPrelude brings gqlparser's introspection schema up to the current
// specification. The prelude predates directive deprecation, so the fields
// below are absent from every schema it loads; a client that asks for them
// gets a validation error rather than the {false, null} the specification
// requires. gqlparser cannot yet parse DIRECTIVE_DEFINITION as a location,
// so nothing can actually be deprecated -- but the shape must still be there.
func patchPrelude(s *ast.Schema) {
	if d := s.Types["__Directive"]; d != nil {
		if d.Fields.ForName("isDeprecated") == nil {
			d.Fields = append(d.Fields,
				&ast.FieldDefinition{Name: "isDeprecated", Type: ast.NonNullNamedType("Boolean", nil)},
				&ast.FieldDefinition{Name: "deprecationReason", Type: ast.NamedType("String", nil)},
			)
		}
	}
	if loc := s.Types["__DirectiveLocation"]; loc != nil {
		if loc.EnumValues.ForName(string(locationDirectiveDefinition)) == nil {
			loc.EnumValues = append(loc.EnumValues, &ast.EnumValueDefinition{Name: string(locationDirectiveDefinition)})
		}
	}
	// The prelude also declares @defer, which is neither implemented here nor
	// part of the specification. Dropping it makes the validator agree with
	// what introspection already advertises, so a client asking for
	// incremental delivery is told no instead of silently getting a whole
	// response. Only the prelude's declaration goes: a schema is free to
	// define a @defer of its own, and that one is the author's business.
	if d := s.Directives["defer"]; d != nil && isBuiltInDefinition(d.Position) {
		delete(s.Directives, "defer")
	}
	// gqlparser's prelude is missing a space in String's description: it reads
	// "The `String`scalar type". Every introspection response carries it, and
	// every tool that renders schema documentation shows it, so it is worth the
	// one line here rather than only upstream. Matched exactly so a fixed
	// prelude silently stops needing this instead of corrupting the fixed text.
	if t := s.Types["String"]; t != nil && isBuiltInDefinition(t.Position) {
		t.Description = strings.Replace(t.Description, "`String`scalar", "`String` scalar", 1)
	}
	if sc := s.Types["__Schema"]; sc != nil {
		if f := sc.Fields.ForName("directives"); f != nil && f.Arguments.ForName("includeDeprecated") == nil {
			f.Arguments = append(f.Arguments, &ast.ArgumentDefinition{
				Name:         "includeDeprecated",
				Type:         ast.NamedType("Boolean", nil),
				DefaultValue: &ast.Value{Raw: "false", Kind: ast.BooleanValue},
			})
		}
	}
}

// isBuiltInDefinition reports whether a definition came from gqlparser's
// prelude rather than from the caller's SDL.
func isBuiltInDefinition(pos *ast.Position) bool {
	return pos != nil && pos.Src != nil && pos.Src.BuiltIn
}

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

// directives returns the schema's directives. gqlparser has no place to hang
// directives applied to a directive definition, so none is ever deprecated and
// includeDeprecated cannot yet change the result.
func (s *introSchema) directives(a includeDeprecatedArgs) []*introDirective {
	names := make([]string, 0, len(s.s.Directives))
	for name := range s.s.Directives {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]*introDirective, 0, len(names))
	for _, name := range names {
		d := &introDirective{s: s.s, def: s.s.Directives[name]}
		if !a.include() && isDeprecated(d.directives()) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// directives returns the directives applied to this directive definition.
// gqlparser's AST has no field for them yet, so the list is always empty.
func (d *introDirective) directives() ast.DirectiveList { return nil }

func (v *introInputValue) defaultValueString() *string {
	if v.defaultValue == nil {
		return nil
	}
	str := sdlprint.Value(v.defaultValue)
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
			FieldArgs("directives", (*introSchema).directives),
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
			Field("isDeprecated", func(d *introDirective) bool { return isDeprecated(d.directives()) }),
			Field("deprecationReason", func(d *introDirective) *string { _, r := deprecation(d.directives()); return r }),
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
