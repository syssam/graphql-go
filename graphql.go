package graphql

import (
	"encoding/json"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// ID is the default Go representation of the GraphQL ID scalar.
type ID string

// Root is the parent value passed to fields of the Query, Mutation and
// Subscription root types.
type Root struct{}

// Omittable distinguishes an input field that was absent from one that was
// explicitly set to null. Use it on PATCH-style input structs; Auto
// bindings and OmittableField both understand it.
type Omittable[T any] struct {
	value T
	set   bool
}

// OmittableOf returns an Omittable that is set to v.
func OmittableOf[T any](v T) Omittable[T] {
	return Omittable[T]{value: v, set: true}
}

// IsSet reports whether the field was present in the input, even if its
// value was null.
func (o Omittable[T]) IsSet() bool { return o.set }

// Value returns the decoded value, or the zero value when unset.
func (o Omittable[T]) Value() T { return o.value }

// ValueOK returns the decoded value together with whether it was set.
func (o Omittable[T]) ValueOK() (T, bool) { return o.value, o.set }

// Or returns the set value, or def when the field was absent.
func (o Omittable[T]) Or(def T) T {
	if o.set {
		return o.value
	}
	return def
}

// MarshalJSON encodes the value, or null when unset.
func (o Omittable[T]) MarshalJSON() ([]byte, error) {
	if !o.set {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}

// UnmarshalJSON decodes into the value and marks it set. A field absent from the JSON never reaches
// this method, so it stays unset; an explicit null does, and is set with the zero value. That is
// the distinction Omittable exists for, and it lets an input struct be filled from JSON (a stored
// definition, a test fixture) as well as from a GraphQL request.
func (o *Omittable[T]) UnmarshalJSON(b []byte) error {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.value, o.set = v, true
	return nil
}

// assign is used by Auto input bindings. Application code should keep
// using OmittableOf / IsSet / Value.
func (o *Omittable[T]) assign(v any, set bool) {
	o.set = set
	if !set || v == nil {
		var z T
		o.value = z
		return
	}
	o.value = v.(T)
}

// omittableAssigner is implemented by *Omittable[T] for every T.
type omittableAssigner interface {
	assign(v any, set bool)
}

// Writer is handed to scalar marshalers to emit exactly one JSON value.
// Conversions to and from the internal writer are free, so custom scalars
// add no allocation on the hot path.
type Writer jsonw.Writer

func (w *Writer) inner() *jsonw.Writer { return (*jsonw.Writer)(w) }

// String writes a JSON string.
func (w *Writer) String(s string) { w.inner().String(s) }

// Int64 writes a JSON number.
func (w *Writer) Int64(v int64) { w.inner().Int64(v) }

// Uint64 writes a JSON number.
func (w *Writer) Uint64(v uint64) { w.inner().Uint64(v) }

// Float64 writes a JSON number. It returns an error for NaN and infinities.
func (w *Writer) Float64(v float64) error { return w.inner().Float64(v) }

// Bool writes true or false.
func (w *Writer) Bool(v bool) { w.inner().Bool(v) }

// Null writes null.
func (w *Writer) Null() { w.inner().Null() }

// Raw writes an already-encoded JSON value. The caller is responsible for
// its validity.
func (w *Writer) Raw(b []byte) { w.inner().Raw(b) }
