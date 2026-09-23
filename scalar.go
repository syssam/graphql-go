package graphql

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Scalar binds the GraphQL scalar name to the Go type T. marshal writes the
// output representation; unmarshal coerces an input value, which is one of
// string, bool, json.Number, int64, float64, []any or map[string]any.
// Registering a Go type for a built-in scalar (for example ID as uuid.UUID)
// adds to the built-in mappings rather than replacing them.
//
// unmarshal receives the same representation whether the value arrived as a
// literal in the query or as a variable -- a number is a json.Number carrying
// its original text either way, so a decimal or an id keeps the digits the
// client sent.
//
// **Write it pure and cheap.** It runs twice for a value supplied as a
// variable: once from coerceVariables, which decodes to find out whether the
// value is acceptable and discards the result so a variable error is reported
// before execution starts, and once when the argument is decoded for real. A
// literal runs it once.
func Scalar[T any](name string, marshal func(*Writer, T) error, unmarshal func(any) (T, error)) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Types[name]
		if def == nil || def.Kind != ast.Scalar {
			b.errorf("Scalar %q: type is not a scalar in the schema", name)
			return
		}
		registerLeaf(b.reg, name, ast.Scalar, func(w *jsonw.Writer, v T) error {
			return marshal((*Writer)(w), v)
		}, unmarshal)
	})
}

// registerBuiltins installs the specification's scalars for their natural Go
// types.
func registerBuiltins(b *schemaBuilder) {
	r := b.reg
	registerAbstractShapes[any](r)

	registerLeaf(r, "Int", ast.Scalar, writeInt[int], decodeInt[int])
	registerLeaf(r, "Int", ast.Scalar, writeInt[int32], decodeInt[int32])
	registerLeaf(r, "Int", ast.Scalar, writeInt[int64], decodeInt[int64])

	registerLeaf(r, "Float", ast.Scalar, func(w *jsonw.Writer, v float64) error {
		if err := w.Float64(v); err != nil {
			return fmt.Errorf("Float cannot represent non numeric value: %v", v)
		}
		return nil
	}, decodeFloat64)
	registerLeaf(r, "Float", ast.Scalar, func(w *jsonw.Writer, v float32) error {
		if err := w.Float64(float64(v)); err != nil {
			return fmt.Errorf("Float cannot represent non numeric value: %v", v)
		}
		return nil
	}, func(raw any) (float32, error) {
		f, err := decodeFloat64(raw)
		return float32(f), err
	})

	registerLeaf(r, "String", ast.Scalar, func(w *jsonw.Writer, v string) error {
		w.String(v)
		return nil
	}, decodeString)

	registerLeaf(r, "Boolean", ast.Scalar, func(w *jsonw.Writer, v bool) error {
		w.Bool(v)
		return nil
	}, decodeBool)

	registerLeaf(r, "ID", ast.Scalar, func(w *jsonw.Writer, v ID) error {
		w.String(string(v))
		return nil
	}, func(raw any) (ID, error) {
		s, err := decodeIDString(raw)
		return ID(s), err
	})
	registerLeaf(r, "ID", ast.Scalar, func(w *jsonw.Writer, v string) error {
		w.String(v)
		return nil
	}, decodeIDString)
	registerLeaf(r, "ID", ast.Scalar, func(w *jsonw.Writer, v int64) error {
		w.String(strconv.FormatInt(v, 10))
		return nil
	}, decodeIDInt[int64])
	registerLeaf(r, "ID", ast.Scalar, func(w *jsonw.Writer, v int) error {
		w.String(strconv.Itoa(v))
		return nil
	}, decodeIDInt[int])
}

type integer interface{ ~int | ~int32 | ~int64 }

func writeInt[T integer](w *jsonw.Writer, v T) error {
	n := int64(v)
	if n < math.MinInt32 || n > math.MaxInt32 {
		return fmt.Errorf("Int cannot represent non 32-bit signed integer value: %d", n)
	}
	w.Int64(n)
	return nil
}

func decodeInt[T integer](raw any) (T, error) {
	n, err := rawInt64(raw, "Int")
	if err != nil {
		return 0, err
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("Int cannot represent non 32-bit signed integer value: %d", n)
	}
	return T(n), nil
}

// rawInt64 accepts integral JSON numbers and Go integers. Floats with a zero
// fractional part are accepted because JSON does not distinguish 1.0 from 1.
func rawInt64(raw any, scalar string) (int64, error) {
	switch v := raw.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		f, err := v.Float64()
		if err != nil || f != math.Trunc(f) || math.IsInf(f, 0) || math.Abs(f) > math.MaxInt64 {
			return 0, fmt.Errorf("%s cannot represent non-integer value: %s", scalar, v)
		}
		return int64(f), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case float64:
		if v != math.Trunc(v) || math.IsInf(v, 0) || math.IsNaN(v) {
			return 0, fmt.Errorf("%s cannot represent non-integer value: %v", scalar, v)
		}
		return int64(v), nil
	}
	return 0, fmt.Errorf("%s cannot represent non-integer value: %s", scalar, describeRaw(raw))
}

func decodeFloat64(raw any) (float64, error) {
	switch v := raw.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("Float cannot represent non numeric value: %s", v)
		}
		return f, nil
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	}
	return 0, fmt.Errorf("Float cannot represent non numeric value: %s", describeRaw(raw))
}

func decodeString(raw any) (string, error) {
	if s, ok := raw.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("String cannot represent a non string value: %s", describeRaw(raw))
}

func decodeBool(raw any) (bool, error) {
	if b, ok := raw.(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("Boolean cannot represent a non boolean value: %s", describeRaw(raw))
}

func decodeIDString(raw any) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return v.String(), nil
		}
	case int64:
		return strconv.FormatInt(v, 10), nil
	case int:
		return strconv.Itoa(v), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	}
	return "", fmt.Errorf("ID cannot represent value: %s", describeRaw(raw))
}

func decodeIDInt[T integer](raw any) (T, error) {
	if s, ok := raw.(string); ok {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("ID cannot represent value: %q", s)
		}
		return T(n), nil
	}
	n, err := rawInt64(raw, "ID")
	if err != nil {
		return 0, fmt.Errorf("ID cannot represent value: %s", describeRaw(raw))
	}
	return T(n), nil
}

// describeRaw renders an input value for error messages.
func describeRaw(raw any) string {
	switch v := raw.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(v)
	case json.Number:
		return v.String()
	case bool, int, int32, int64, float64, float32:
		return fmt.Sprint(v)
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", raw)
}
