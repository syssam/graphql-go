package graphql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// coerceVariables implements CoerceVariableValues from the specification:
// missing variables take their default, non-null variables must be present
// and non-null, and every provided value is validated against its declared
// input type. The returned map holds raw (still untyped) values that the
// argument decoders consume.
func (s *Schema) coerceVariables(op *ast.OperationDefinition, raw map[string]any) (map[string]any, *Error) {
	out := make(map[string]any, len(op.VariableDefinitions))
	for _, vd := range op.VariableDefinitions {
		v, has := raw[vd.Variable]
		if !has {
			if vd.DefaultValue != nil {
				dv, err := astJSON(vd.DefaultValue, nil)
				if err != nil {
					return nil, variableError(vd, "has an invalid default value: %v", err)
				}
				out[vd.Variable] = dv
				continue
			}
			if vd.Type.NonNull {
				return nil, variableError(vd, "of required type %q was not provided.", vd.Type.String())
			}
			continue
		}
		if v == nil {
			if vd.Type.NonNull {
				return nil, variableError(vd, "of non-null type %q must not be null.", vd.Type.String())
			}
			out[vd.Variable] = nil
			continue
		}
		if !s.validInput(vd.Type, v) {
			if err := s.validateInput(vd.Type, v, "$"+vd.Variable); err != nil {
				return nil, variableError(vd, "got invalid value %s; %v", describeJSON(v), err)
			}
		}
		out[vd.Variable] = v
	}
	return out, nil
}

// variableDefaultErrors reports every variable default in doc that does not
// coerce to its variable's type. gqlparser checks a default's kind -- a string
// for an Int is refused -- and not its value, so an Int out of range or a
// custom scalar's own refusal reached execution: the operation ran, and the
// default failed whichever field used it, where the same value written as a
// literal argument is a validation error.
func (s *Schema) variableDefaultErrors(doc *ast.QueryDocument) []*Error {
	var errs []*Error
	for _, op := range doc.Operations {
		for _, vd := range op.VariableDefinitions {
			if vd.DefaultValue == nil {
				continue
			}
			dv, err := astJSON(vd.DefaultValue, nil)
			if err == nil && dv != nil && !s.validInput(vd.Type, dv) {
				err = s.validateInput(vd.Type, dv, "$"+vd.Variable)
			}
			if err != nil {
				errs = append(errs, variableError(vd, "has an invalid default value: %v", err).WithCode(CodeValidationFailed))
			}
		}
	}
	return errs
}

func variableError(vd *ast.VariableDefinition, format string, args ...any) *Error {
	e := Errorf("Variable \"$%s\" "+format, append([]any{vd.Variable}, args...)...).WithCode(CodeBadUserInput)
	if vd.Position != nil {
		e.Locations = []Location{{Line: vd.Position.Line, Column: vd.Position.Column}}
	}
	return e
}

// validInput is validateInput's verdict without its message: it builds no
// path, and visits an input object's own keys rather than every declared
// field, which for an ent-style WhereInput is two against a hundred. It may
// refuse more than validateInput does, never less; a refusal is re-checked
// there, which also gives the error the same wording and order as ever.
func (s *Schema) validInput(t *ast.Type, v any) bool {
	if v == nil {
		return !t.NonNull
	}
	if t.Elem != nil {
		items, isList := v.([]any)
		if !isList {
			return s.validInput(t.Elem, v)
		}
		for _, it := range items {
			if !s.validInput(t.Elem, it) {
				return false
			}
		}
		return true
	}
	in := s.inputs[t.NamedType]
	if in == nil {
		// Not an input object: scalars and enums hold no path to build,
		// so the full check costs nothing extra.
		return s.validateInput(t, v, "") == nil
	}
	m, ok := v.(map[string]any)
	if !ok || (in.oneOf && len(m) != 1) {
		return false
	}
	for name, fv := range m {
		fd := in.fields[name]
		if fd == nil || (in.oneOf && fv == nil) || !s.validInput(fd.Type, fv) {
			return false
		}
	}
	for _, name := range in.required {
		if _, present := m[name]; !present {
			return false
		}
	}
	return true
}

// inputInfo is what validInput needs of an input object, indexed once.
type inputInfo struct {
	fields   map[string]*ast.FieldDefinition
	required []string // non-null with no default
	oneOf    bool
}

func indexInputs(schema *ast.Schema) map[string]*inputInfo {
	out := map[string]*inputInfo{}
	for name, def := range schema.Types {
		if def.Kind != ast.InputObject {
			continue
		}
		in := &inputInfo{fields: make(map[string]*ast.FieldDefinition, len(def.Fields)), oneOf: def.Directives.ForName("oneOf") != nil}
		for _, fd := range def.Fields {
			in.fields[fd.Name] = fd
			if fd.Type.NonNull && fd.DefaultValue == nil {
				in.required = append(in.required, fd.Name)
			}
		}
		out[name] = in
	}
	return out
}

// validateInput checks a raw JSON-shaped value against an input type without
// producing a typed value. It mirrors the checks the typed decoders perform
// so that variable errors are reported before execution starts.
func (s *Schema) validateInput(t *ast.Type, v any, path string) error {
	if v == nil {
		if t.NonNull {
			return fmt.Errorf("Expected non-nullable type %q not to be null at %s.", t.String(), path)
		}
		return nil
	}
	if t.Elem != nil {
		items, isList := v.([]any)
		if !isList {
			return s.validateInput(t.Elem, v, path)
		}
		for i, it := range items {
			if err := s.validateInput(t.Elem, it, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		return nil
	}
	def := s.ast.Types[t.NamedType]
	if def == nil {
		return fmt.Errorf("unknown type %q", t.NamedType)
	}
	switch def.Kind {
	case ast.Scalar:
		if validate := s.reg.leafValidators[def.Name]; validate != nil {
			if err := validate(v); err != nil {
				return fmt.Errorf("%w at %s.", err, path)
			}
		}
		return nil
	case ast.Enum:
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("Enum %q cannot represent non-string value: %s at %s.", def.Name, describeRaw(v), path)
		}
		if def.EnumValues.ForName(str) == nil {
			return fmt.Errorf("Value %q does not exist in %q enum at %s.", str, def.Name, path)
		}
		return nil
	case ast.InputObject:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("Expected type %q to be an object at %s.", def.Name, path)
		}
		// A OneOf Input Object arriving whole from a variable is never seen by
		// the validator, which only inspects literals.
		if def.Directives.ForName("oneOf") != nil {
			if len(m) != 1 {
				return fmt.Errorf("OneOf Input Object %q must specify exactly one key at %s.", def.Name, path)
			}
			for name, fv := range m {
				if fv == nil {
					return fmt.Errorf("Field %q must be non-null at %s.", def.Name+"."+name, path)
				}
			}
		}
		for name := range m {
			if def.Fields.ForName(name) == nil {
				return fmt.Errorf("Field %q is not defined by type %q at %s.", name, def.Name, path)
			}
		}
		for _, fd := range def.Fields {
			fv, present := m[fd.Name]
			if !present {
				if fd.Type.NonNull && fd.DefaultValue == nil {
					return fmt.Errorf("Field %q of required type %q was not provided at %s.", fd.Name, fd.Type.String(), path)
				}
				continue
			}
			if err := s.validateInput(fd.Type, fv, path+"."+fd.Name); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("type %q is not an input type", def.Name)
}

// describeJSON renders a raw value compactly for error messages.
func describeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return describeRaw(v)
	}
	const limit = 200
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}

// decodeVariables parses the request's variables JSON with number precision
// preserved.
func decodeVariables(raw json.RawMessage) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var vars map[string]any
	if err := dec.Decode(&vars); err != nil {
		return nil, fmt.Errorf("variables must be a JSON object: %w", err)
	}
	if vars == nil {
		vars = map[string]any{}
	}
	return vars, nil
}

// valueHasVariables reports whether an argument value references a variable
// anywhere in its tree.
func valueHasVariables(v *ast.Value) bool {
	if v == nil {
		return false
	}
	if v.Kind == ast.Variable {
		return true
	}
	for _, c := range v.Children {
		if valueHasVariables(c.Value) {
			return true
		}
	}
	return false
}

// astJSON evaluates an AST value the way a transport would have decoded
// JSON: integers and floats become json.Number so custom scalars see the
// same representation for literals and variables.
func astJSON(v *ast.Value, vars map[string]any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch v.Kind {
	case ast.Variable:
		return v.Value(vars)
	case ast.IntValue, ast.FloatValue:
		return json.Number(v.Raw), nil
	case ast.StringValue, ast.BlockValue, ast.EnumValue:
		return v.Raw, nil
	case ast.BooleanValue:
		return strconv.ParseBool(v.Raw)
	case ast.NullValue:
		return nil, nil
	case ast.ListValue:
		out := make([]any, 0, len(v.Children))
		for _, c := range v.Children {
			elem, err := astJSON(c.Value, vars)
			if err != nil {
				return nil, err
			}
			out = append(out, elem)
		}
		return out, nil
	case ast.ObjectValue:
		out := make(map[string]any, len(v.Children))
		for _, c := range v.Children {
			if c.Value != nil && c.Value.Kind == ast.Variable {
				if _, ok := vars[c.Value.Raw]; !ok {
					continue
				}
			}
			elem, err := astJSON(c.Value, vars)
			if err != nil {
				return nil, err
			}
			out[c.Name] = elem
		}
		return out, nil
	default:
		return v.Value(vars)
	}
}

// fieldArguments returns the field's arguments with defaults applied and
// numbers normalised to json.Number.
//
// It walks the AST itself rather than calling ast.Field.ArgumentMap, which
// evaluates every literal through gqlparser's Value. That does two things this
// cannot have. It **panics** on an integer literal wider than int64 --
// strconv.ParseInt out of range -- and validation cannot save us there: Int
// and Float reject an oversized literal, but a custom scalar accepts anything
// by definition, so `{ f(v: 123456789012345678901234567890) }` against any
// schema declaring a custom scalar argument is a client-supplied panic on the
// request path, reachable at plan compile as well as at execution. And it
// rounds a float literal through float64, so 9007199254740993.0 reaches a
// custom scalar as 9.007199254740992e+15 inlined and exactly as written when
// sent as a variable -- the same value decoding differently depending on
// whether the client parameterised it, which is precisely what astJSON exists
// to prevent for a Decimal or a big-integer scalar.
//
// The walk below is arg2map's own logic with astJSON in place of Value: an
// argument the query supplies wins, a variable that is absent from vars leaves
// the argument unset so its default applies, and everything else falls back to
// the definition's default. astJSON keeps an integer or float literal as
// json.Number carrying its original text, so neither failure is reachable.
func fieldArguments(f *ast.Field, vars map[string]any) (map[string]any, error) {
	defs := f.Definition.Arguments
	if len(defs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(defs))
	for _, def := range defs {
		var val any
		var has bool
		if supplied := f.Arguments.ForName(def.Name); supplied != nil {
			if supplied.Value.Kind == ast.Variable {
				val, has = vars[supplied.Value.Raw]
			} else {
				v, err := astJSON(supplied.Value, vars)
				if err != nil {
					return nil, fmt.Errorf("argument %q: %w", def.Name, err)
				}
				val, has = v, true
			}
		}
		if !has && def.DefaultValue != nil {
			v, err := astJSON(def.DefaultValue, vars)
			if err != nil {
				return nil, fmt.Errorf("argument %q default: %w", def.Name, err)
			}
			val, has = v, true
		}
		if has {
			out[def.Name] = val
		}
	}
	return out, nil
}
