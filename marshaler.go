package graphql

import (
	"errors"
	"fmt"
	"io"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Marshaler is the encoding contract gqlgen defines and that ent-, velox- and
// gqlgen-generated leaf types implement: MarshalGQL writes the value's JSON
// encoding, UnmarshalGQL decodes an input value into the receiver. PT is *T;
// MarshalGQL may have either receiver.
type Marshaler[T any] interface {
	*T
	MarshalGQL(w io.Writer)
	UnmarshalGQL(v any) error
}

// EnumMarshaler binds the GraphQL enum name to a Go type that encodes itself.
//
// It is for enum types Enum cannot take: an ORM's order field is a struct
// holding a cursor function, not comparable, whose values are unexported and
// constructed only by UnmarshalGQL. Output is checked against the enum's
// declared values, so a MarshalGQL naming an undeclared one is a field error,
// as it is for Enum.
func EnumMarshaler[T any, PT Marshaler[T]](name string) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		declared := declaredEnumValues(b, name)
		if declared == nil {
			return
		}
		registerLeaf(b.reg, name, ast.Enum, func(w *jsonw.Writer, v T) error {
			start := w.Len()
			return checkEnumOutput(w, start, marshalGQL[T, PT](w, v), declared, name)
		}, func(raw any) (T, error) {
			var v T
			if err := checkEnumInput(raw, name); err != nil {
				return v, err
			}
			err := PT(&v).UnmarshalGQL(raw)
			return v, err
		})
	})
}

// declaredEnumValues is the set of name's values as JSON strings, or nil
// when name is not an enum in the schema.
func declaredEnumValues(b *schemaBuilder, name string) map[string]bool {
	def := b.ast.Types[name]
	if def == nil || def.Kind != ast.Enum {
		b.errorf("EnumMarshaler %q: type is not an enum in the schema", name)
		return nil
	}
	declared := make(map[string]bool, len(def.EnumValues))
	for _, ev := range def.EnumValues {
		declared[string(jsonw.AppendString(nil, ev.Name))] = true
	}
	return declared
}

// checkEnumOutput reports what MarshalGQL wrote from start, having returned
// err, if it is not one of the enum's declared values.
func checkEnumOutput(w *jsonw.Writer, start int, err error, declared map[string]bool, name string) error {
	if err != nil {
		return fmt.Errorf("enum %s: %w", name, err)
	}
	if out := w.Bytes()[start:]; !declared[string(trimSep(out))] {
		return fmt.Errorf("enum %s cannot represent value: %s", name, trimSep(out))
	}
	return nil
}

func checkEnumInput(raw any, name string) error {
	if _, ok := raw.(string); !ok {
		return fmt.Errorf("enum %s cannot represent non-string value: %s", name, describeRaw(raw))
	}
	return nil
}

// ScalarMarshaler binds the custom scalar name to a Go type that encodes
// itself -- a Relay cursor, a decimal, anything a generator emitted with
// MarshalGQL and UnmarshalGQL. It is Scalar with the two functions taken from
// the type.
func ScalarMarshaler[T any, PT Marshaler[T]](name string) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		def := b.ast.Types[name]
		if def == nil || def.Kind != ast.Scalar {
			b.errorf("ScalarMarshaler %q: type is not a scalar in the schema", name)
			return
		}
		registerLeaf(b.reg, name, ast.Scalar, func(w *jsonw.Writer, v T) error {
			if err := marshalGQL[T, PT](w, v); err != nil {
				return fmt.Errorf("scalar %s: %w", name, err)
			}
			return nil
		}, func(raw any) (T, error) {
			var v T
			err := PT(&v).UnmarshalGQL(raw)
			return v, err
		})
	})
}

// marshalGQL runs MarshalGQL into w as one JSON value, however many Write
// calls it makes: the first begins the value, with its separator, and the
// rest continue it.
func marshalGQL[T any, PT Marshaler[T]](w *jsonw.Writer, v T) error {
	mw := gqlValueWriter{w: w}
	PT(&v).MarshalGQL(&mw)
	if !mw.started {
		return errors.New("MarshalGQL wrote nothing")
	}
	return nil
}

type gqlValueWriter struct {
	w       *jsonw.Writer
	started bool
}

func (g *gqlValueWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !g.started {
		g.w.Raw(p)
		g.started = true
	} else {
		g.w.RawContinue(p)
	}
	return len(p), nil
}

// trimSep drops the separator Raw wrote before the value, if any.
func trimSep(b []byte) []byte {
	if len(b) > 0 && (b[0] == ',' || b[0] == ':') {
		return b[1:]
	}
	return b
}
