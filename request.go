package graphql

import (
	"bytes"
	"context"
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

	// QueryOnly makes Execute refuse a mutation. A transport sets it on a
	// request that must not change state -- an HTTP GET, which a link, a
	// prefetch or a cache may replay -- and the executor enforces it on the
	// operation it is about to run, after every RequestInterceptor. That is
	// the only place it can be enforced: an interceptor may replace Query, as
	// a persisted-query lookup does, so the text a transport received is not
	// always the text that runs.
	//
	// A refused request carries no data and one error whose code is
	// CodeQueryOnly. That error does not pass through the ErrorPresenter,
	// because a transport finds the refusal by its code to answer with a
	// status of its own. Subscribe ignores the field: it runs nothing but
	// subscriptions. It is never read from a request body. An interceptor
	// that hands next a Request of its own need not copy it: Execute carries
	// it in the context it gives the chain, so it is lost only by an
	// interceptor that also replaces that context.
	QueryOnly bool `json:"-"`
}

// queryOnlyKey marks, in the context Execute hands the request chain, that
// the request it was given was QueryOnly.
type queryOnlyKey struct{}

// queryOnly reports whether the request Execute was given was QueryOnly. The
// one that reaches the innermost handler is not always that request: an
// interceptor resolving a stored document may build a Request of its own, and
// the flag it left behind is exactly the one a GET depends on. The context is
// only asked when there is an interceptor to have done so, and only for an
// operation that is not a query.
func (e *Executor) queryOnly(ctx context.Context, req *Request) bool {
	return req.QueryOnly || (len(e.reqInterceptors) > 0 && ctx.Value(queryOnlyKey{}) != nil)
}

// queryOnlyRefusal is the response to a mutation on a QueryOnly request. It
// is built unpresented, like the WithMaxErrors notice: the code is how a
// transport tells this refusal from a request error, and a presenter that
// rewrote the error would turn a 405 into whatever the media type gives.
func queryOnlyRefusal() *Response {
	return &Response{Errors: []*Error{
		Errorf("This request may only run a query; send a mutation with POST.").WithCode(CodeQueryOnly),
	}}
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

// WriteTo writes the JSON response envelope to w with errors, data and
// extensions in that order, omitting members that are empty. Data is written
// straight from the execution buffer without an intermediate copy.
//
// It returns an error, and writes nothing, when an extension value on the
// response or on any error does not marshal. Nothing is written rather than a
// truncated envelope, so a caller that checks the error can still send
// something of its own -- but the HTTP transports discover it after the status
// header is on the wire and can only log, which leaves the client an empty
// body. See Error.WithExtension.
func (r *Response) WriteTo(w io.Writer) (int64, error) {
	head, tail, err := r.envelope()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, part := range [][]byte{head, r.Data, tail} {
		if len(part) == 0 {
			continue
		}
		n, err := w.Write(part)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// envelope renders the bytes preceding and following Data.
func (r *Response) envelope() (head, tail []byte, err error) {
	head = append(head, '{')
	first := true
	if len(r.Errors) > 0 {
		head = append(head, `"errors":[`...)
		for i, e := range r.Errors {
			if i > 0 {
				head = append(head, ',')
			}
			if head, err = e.appendJSON(head); err != nil {
				return nil, nil, err
			}
		}
		head = append(head, ']')
		first = false
	}
	if r.Data != nil {
		if !first {
			head = append(head, ',')
		}
		head = append(head, `"data":`...)
		first = false
	}
	if len(r.Extensions) > 0 {
		if !first {
			tail = append(tail, ',')
		}
		tail = append(tail, `"extensions":`...)
		ext, err := json.Marshal(r.Extensions)
		if err != nil {
			return nil, nil, err
		}
		tail = append(tail, ext...)
	}
	tail = append(tail, '}')
	return head, tail, nil
}

// MarshalJSON renders the response envelope as a single byte slice.
func (r *Response) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(r.Data) + 64)
	if _, err := r.WriteTo(&buf); err != nil {
		return nil, err
	}
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
