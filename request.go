package graphql

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/syssam/graphql-go/internal/jsonw"
)

// Request is a GraphQL request as delivered by a transport.
type Request struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName,omitempty"`
	Variables     json.RawMessage `json:"variables,omitempty"`
	Extensions    map[string]any  `json:"extensions,omitempty"`
}

// Response is the result of executing a Request.
//
// Data holds the already-encoded "data" member, or nil when the request
// failed before execution began. Call Release once the response has been
// written to return its buffer to the pool.
type Response struct {
	Data       []byte
	Errors     []*Error
	Extensions map[string]any

	buf *jsonw.Writer
}

// HasRequestErrors reports whether the request failed before execution, in
// which case the response carries errors and no data.
func (r *Response) HasRequestErrors() bool {
	return r.Data == nil && len(r.Errors) > 0
}

// WriteTo writes the JSON response envelope to w.
func (r *Response) WriteTo(w io.Writer) (int64, error) {
	b, err := r.MarshalJSON()
	if err != nil {
		return 0, err
	}
	n, err := w.Write(b)
	return int64(n), err
}

// MarshalJSON renders the response envelope with errors, data and extensions
// in that order, omitting members that are empty.
func (r *Response) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(r.Data) + 64)
	buf.WriteByte('{')
	first := true
	if len(r.Errors) > 0 {
		buf.WriteString(`"errors":[`)
		for i, e := range r.Errors {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, err := e.appendJSON(nil)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		first = false
	}
	if r.Data != nil {
		if !first {
			buf.WriteByte(',')
		}
		buf.WriteString(`"data":`)
		buf.Write(r.Data)
		first = false
	}
	if len(r.Extensions) > 0 {
		if !first {
			buf.WriteByte(',')
		}
		buf.WriteString(`"extensions":`)
		ext, err := json.Marshal(r.Extensions)
		if err != nil {
			return nil, err
		}
		buf.Write(ext)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Release returns the response buffer to the pool. Data must not be used
// afterwards. Calling Release more than once is a no-op.
func (r *Response) Release() {
	if r.buf == nil {
		return
	}
	jsonw.Put(r.buf)
	r.buf = nil
	r.Data = nil
}
