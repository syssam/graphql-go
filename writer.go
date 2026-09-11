package graphql

import "github.com/syssam/graphql-go/internal/jsonw"

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
