package graphql

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Scalar binds the GraphQL scalar name to the Go type T. marshal writes the
// output representation; unmarshal coerces an input value, which is one of
// string, bool, json.Number, int64, float64, []any or map[string]any.
// Registering a Go type for a built-in scalar (for example ID as uuid.UUID)
// adds to the built-in mappings rather than replacing them. A Go type the
// built-in already binds -- string for String or ID, int64 for Int or ID --
// is a build error, as any second binding of one pair is.
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
// literal runs it once, when its plan is compiled, unless the decoded argument
// holds a slice, a map or another value two requests could share; then it runs
// again for each request.
//
// marshal must write exactly one JSON value. Writing none, or more than one,
// is a field error naming the scalar, and what it wrote is discarded. A null
// input never reaches unmarshal, and when T is an interface type a nil value
// never reaches marshal: it is null, and an error at a non-null position.
// Several Go types may be bound to one scalar, one call each; the same Go type
// twice is a build error. ScalarMarshaler is the binding for a type that
// carries MarshalGQL and UnmarshalGQL.
func Scalar[T any](name string, marshal func(*Writer, T) error, unmarshal func(any) (T, error)) SchemaOption {
	return bindScalar("Scalar", name, func(w *Writer, v T) error {
		jw := w.inner()
		start := jw.Mark()
		jw.BeginLeaf()
		return checkScalarOutput(jw, start, marshal(w, v), name, true)
	}, unmarshal)
}

var errNotOneValue = errors.New("marshal must write exactly one JSON value")

// checkScalarOutput reports what a custom scalar's marshal wrote since start,
// having returned err, unless it is exactly one JSON value, and rewinds to
// start then. Raw trusts its caller, so a marshal that writes nothing, or two
// values, or a string it did not quote, would otherwise corrupt the whole
// response instead of failing one field.
//
// counted says the marshal wrote through Writer, which counts its values:
// one written by String, Int64 and the rest is a JSON value by construction,
// and reading it back doubled the cost of a list of them. Only bytes that
// came through Raw, or from a MarshalGQL, are read.
//
// It is not generic, so Scalar and ScalarMarshaler, which generated code
// instantiates once per type, share the one copy.
func checkScalarOutput(w *jsonw.Writer, start jsonw.Mark, err error, name string, counted bool) error {
	if err == nil {
		one, read := true, true
		if counted {
			values, raw := w.Leaf()
			one, read = values == 1, raw
		}
		if one && (!read || oneJSONValue(trimSep(w.Bytes()[start.Offset():]))) {
			return nil
		}
		err = errNotOneValue
	}
	w.Rewind(start)
	return fmt.Errorf("scalar %s: %w", name, err)
}

// oneJSONValue reports whether b is exactly one JSON value, as json.Valid
// does. What a scalar writes is nearly always a string with nothing to
// escape, an integer or a literal, and each of those is recognised in one
// pass over the bytes; anything else is json.Valid's to decide.
func oneJSONValue(b []byte) bool {
	n := len(b)
	if n == 0 {
		return false
	}
	switch c := b[0]; {
	case c == '"':
		if n < 2 || b[n-1] != '"' {
			return json.Valid(b)
		}
		return plainStringBody(b[1:n-1]) || json.Valid(b)
	case c == '-' || (c >= '0' && c <= '9'):
		digits := b
		if c == '-' {
			digits = b[1:]
		}
		if len(digits) == 0 || (digits[0] == '0' && len(digits) > 1) {
			return json.Valid(b)
		}
		for _, d := range digits {
			if d < '0' || d > '9' {
				return json.Valid(b)
			}
		}
		return true
	case c == 't' || c == 'f' || c == 'n':
		if string(b) == "true" || string(b) == "false" || string(b) == "null" {
			return true
		}
	}
	return json.Valid(b)
}

// plainStringBody reports whether s could stand between the quotes of a JSON
// string as it is: no control byte, no quote and no backslash. It reads eight
// bytes at a time, since this is run for every value a MarshalGQL writes, and
// a byte at a time cost a list of UUIDs a quarter again. A word that looks
// as if it holds such a byte is only ever a reason to ask json.Valid.
func plainStringBody(s []byte) bool {
	const (
		ones  = 0x0101010101010101
		highs = 0x8080808080808080
	)
	for len(s) >= 8 {
		x := binary.LittleEndian.Uint64(s)
		quote, slash := x^(ones*'"'), x^(ones*'\\')
		// Each term has a high bit set where a byte was below 0x20, a quote
		// or a backslash.
		if ((x-ones*0x20)&^x|(quote-ones)&^quote|(slash-ones)&^slash)&highs != 0 {
			return false
		}
		s = s[8:]
	}
	for _, c := range s {
		if c < 0x20 || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// bindScalar is Scalar without the output check, for marshalers this package
// wrote and so knows to write one value.
func bindScalar[T any](kind, name string, marshal func(*Writer, T) error, unmarshal func(any) (T, error)) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Types[name]
		if def == nil || def.Kind != ast.Scalar {
			b.errorf("%s %q: type is not a scalar in the schema", kind, name)
			return
		}
		if !b.claimLeaf(kind, name, reflect.TypeFor[T]()) {
			return
		}
		registerLeaf(b.reg, name, ast.Scalar, func(w *jsonw.Writer, v T) error {
			return marshal((*Writer)(w), v)
		}, unmarshal)
	})
}

// Time binds the scalar name to time.Time as an RFC 3339 string, the wire
// format of gqlgen's built-in Time, so a service moving over answers with the
// same bytes: the value's own offset is kept, and the fraction is written only
// as far as it is non-zero (time.RFC3339Nano). *time.Time binds with it, and
// nil is null.
//
// Two of gqlgen's choices are not taken. The zero time is written as
// "0001-01-01T00:00:00Z", not null, because null at a Time! position is an
// error the value did not cause; a column that can be empty is a *time.Time.
// And input must be RFC 3339: gqlgen also reads "" as the zero time, which
// hides a missing value, and "2006-01-02 15:04:05" as UTC, which is a
// wall-clock time each client means in its own zone.
func Time(name string) SchemaOption {
	return bindScalar("Scalar", name, marshalTime, unmarshalTime)
}

func marshalTime(w *Writer, t time.Time) error {
	if y := t.Year(); y < 0 || y > 9999 {
		// RFC 3339 has four year digits; writing more would be a value
		// unmarshalTime, and every other reader, refuses.
		return fmt.Errorf("Time cannot represent year %d in RFC 3339", y)
	}
	// Nothing in an RFC 3339 time needs escaping, so the quotes are written
	// around it rather than the time being formatted to a string first.
	var buf [len(time.RFC3339Nano) + 8]byte
	b := append(buf[:0], '"')
	b = append(t.AppendFormat(b, time.RFC3339Nano), '"')
	w.Raw(b)
	return nil
}

func unmarshalTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("Time must be an RFC 3339 string, got %T", v)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("Time must be an RFC 3339 string: %q", s)
	}
	return t, nil
}

// registerBuiltins installs the specification's scalars for their natural Go
// types.
func registerBuiltins(b *schemaBuilder) {
	registerAbstractShapes[any](b.reg)

	builtinLeaf(b, "Int", writeInt[int], decodeInt[int])
	builtinLeaf(b, "Int", writeInt[int32], decodeInt[int32])
	builtinLeaf(b, "Int", writeInt[int64], decodeInt[int64])

	builtinLeaf(b, "Float", func(w *jsonw.Writer, v float64) error {
		if err := w.Float64(v); err != nil {
			return fmt.Errorf("Float cannot represent non numeric value: %v", v)
		}
		return nil
	}, decodeFloat64)
	builtinLeaf(b, "Float", func(w *jsonw.Writer, v float32) error {
		if err := w.Float32(v); err != nil {
			return fmt.Errorf("Float cannot represent non numeric value: %v", v)
		}
		return nil
	}, decodeFloat32)

	builtinLeaf(b, "String", func(w *jsonw.Writer, v string) error {
		w.String(v)
		return nil
	}, decodeString)

	builtinLeaf(b, "Boolean", func(w *jsonw.Writer, v bool) error {
		w.Bool(v)
		return nil
	}, decodeBool)

	builtinLeaf(b, "ID", func(w *jsonw.Writer, v ID) error {
		w.String(string(v))
		return nil
	}, func(raw any) (ID, error) {
		s, err := decodeIDString(raw)
		return ID(s), err
	})
	builtinLeaf(b, "ID", func(w *jsonw.Writer, v string) error {
		w.String(v)
		return nil
	}, decodeIDString)
	builtinLeaf(b, "ID", func(w *jsonw.Writer, v int64) error {
		w.String(strconv.FormatInt(v, 10))
		return nil
	}, decodeIDInt[int64])
	builtinLeaf(b, "ID", func(w *jsonw.Writer, v int) error {
		w.String(strconv.Itoa(v))
		return nil
	}, decodeIDInt[int])
}

// builtinLeaf registers one Go type of a built-in scalar and takes the pair,
// so an option binding the same Go type to it is refused by claimLeaf instead
// of replacing what every field of that type is written and decoded with.
func builtinLeaf[E any](b *schemaBuilder, name string, write func(*jsonw.Writer, E) error, decode func(any) (E, error)) {
	b.leafBound[typeKey{name: name, typ: reflect.TypeFor[E]()}] = boundBuiltIn
	registerLeaf(b.reg, name, ast.Scalar, write, decode)
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

// decodeFloat32 refuses a number float32 cannot hold. The conversion itself
// never fails: past the range it yields an infinity, which would reach the
// resolver as a value no client can send.
func decodeFloat32(raw any) (float32, error) {
	f, err := decodeFloat64(raw)
	if err != nil {
		return 0, err
	}
	n := float32(f)
	if math.IsInf(float64(n), 0) && !math.IsInf(f, 0) {
		return 0, fmt.Errorf("Float cannot represent non 32-bit floating-point value: %s", describeRaw(raw))
	}
	return n, nil
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
