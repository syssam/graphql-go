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
		if err := s.validateInput(vd.Type, v, "$"+vd.Variable); err != nil {
			return nil, variableError(vd, "got invalid value %s; %v", describeJSON(v), err)
		}
		out[vd.Variable] = v
	}
	return out, nil
}

func variableError(vd *ast.VariableDefinition, format string, args ...any) *Error {
	e := Errorf("Variable \"$%s\" "+format, append([]any{vd.Variable}, args...)...).WithCode(CodeBadUserInput)
	if vd.Position != nil {
		e.Locations = []Location{{Line: vd.Position.Line, Column: vd.Position.Column}}
	}
	return e
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
				return fmt.Errorf("%v at %s.", err, path)
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

// asJSON rewrites numbers produced by gqlparser's Value/ArgumentMap into
// json.Number so they match decodeVariables.
func asJSON(v any) any {
	switch x := v.(type) {
	case int:
		return json.Number(strconv.Itoa(x))
	case int32:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int64:
		return json.Number(strconv.FormatInt(x, 10))
	case float32:
		return json.Number(strconv.FormatFloat(float64(x), 'g', -1, 32))
	case float64:
		return json.Number(strconv.FormatFloat(x, 'g', -1, 64))
	case []any:
		for i, e := range x {
			x[i] = asJSON(e)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = asJSON(e)
		}
		return x
	default:
		return v
	}
}

// fieldArguments returns the field's arguments with defaults applied and
// numbers normalised to json.Number.
func fieldArguments(f *ast.Field, vars map[string]any) map[string]any {
	raw := f.ArgumentMap(vars)
	if raw == nil {
		return nil
	}
	return asJSON(raw).(map[string]any)
}
