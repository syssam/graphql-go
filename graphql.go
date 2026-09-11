// Package graphql is a schema-first GraphQL runtime for Go with a code-first,
// type-safe binding API.
//
// A Schema is built from SDL plus a set of bindings that connect GraphQL
// types to Go types: Object, Field, Resolve, Input, Args, Enum, Scalar,
// Interface, Union and Directive. Generated code and hand-written code use the
// same constructors, so code generation is a convenience rather than a
// requirement. Bindings are validated against the SDL once, at NewSchema,
// after which every request runs through typed function values with no
// reflection on the hot path.
//
// An Executor compiles each operation into an immutable, cached plan and
// writes the response directly into a pooled JSON buffer. Fields bound with
// Field are treated as pure data access and run inline; fields bound with
// Resolve may perform I/O and are scheduled concurrently under a bounded
// semaphore.
package graphql

import "encoding/json"

// ID is the default Go representation of the GraphQL ID scalar.
type ID string

// Root is the parent value passed to fields of the Query, Mutation and
// Subscription root types.
type Root struct{}

// Omittable distinguishes an input field that was absent from one that was
// explicitly set to null. Use it with OmittableField for PATCH-style inputs.
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

// MarshalJSON encodes the value, or null when unset.
func (o Omittable[T]) MarshalJSON() ([]byte, error) {
	if !o.set {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}
