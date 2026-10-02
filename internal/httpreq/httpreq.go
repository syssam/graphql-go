// Package httpreq holds the request handling the HTTP transports share:
// query-parameter and JSON-body decoding, the body limit and the CSRF check.
//
// It exists so that gqlhttp and gqlsse cannot drift apart on rules a client
// can tell the difference between. A request rejected as forgeable by one
// transport must be rejected by the other.
package httpreq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/syssam/graphql-go"
)

// MediaTypeJSON is the only body type the transports accept on POST.
const MediaTypeJSON = "application/json"

// Errors reported when a request carries no query and none could be supplied.
var (
	ErrMissingQuery      = errors.New(`request is missing the "query" member.`)
	ErrMissingQueryParam = errors.New(`missing "query" parameter`)
)

// DefaultCSRFHeaders are the headers whose presence marks a request as one
// that required a CORS preflight. Any of them satisfies the CSRF check.
//
// The security property each one has to have is the same: it must be outside
// the CORS safelist, so a browser cannot put it on a cross-origin request
// without a preflight the target origin has to answer. Accept,
// Accept-Language, Content-Language, Content-Type and Range are the safelist;
// everything below is not on it, so every name here costs nothing and each one
// admits a client that would otherwise be refused.
//
// Apollo-Require-Preflight is Apollo Server's own name for this header and is
// what Apollo Client sends, so leaving it out rejected the most widely
// deployed GraphQL client from a GET -- which is the request shape automatic
// persisted queries depend on. WithCSRFPrevention's variadic headers replace
// this list rather than extend it, so an operator who narrows it is opting
// out deliberately.
var DefaultCSRFHeaders = []string{
	"GraphQL-Require-Preflight", // the GraphQL over HTTP specification's name
	"Apollo-Require-Preflight",  // Apollo Server's, sent by Apollo Client
	"X-Requested-With",          // the pre-CORS convention, still sent by many clients
}

// FallbackBody is the envelope a transport sends when serializing the real one
// failed before any byte reached the client. It is valid GraphQL: the
// specification allows a 200 carrying only errors, and a parseable error is
// the one thing better than nothing.
const FallbackBody = `{"errors":[{"message":"internal system error"}]}`

// WriteBody runs write against w and reports whether a fallback envelope is
// needed: true when write failed *and* wrote nothing.
//
// The distinction is the whole point. Response.WriteTo composes the envelope
// before it writes any of it, so a serialization failure -- an extension value
// encoding/json cannot handle -- leaves the body empty and the transport free
// to send something else. A failure *after* bytes have gone out is the client
// disconnecting or the socket erroring, and appending to that would corrupt a
// partly-written response.
//
// Without this a transport that has already sent its status header answers an
// unserializable extension with 200 and an empty body, which a client cannot
// distinguish from success.
func WriteBody(w io.Writer, write func(io.Writer) error) (err error, wroteNothing bool) {
	cw := &countingWriter{w: w}
	err = write(cw)
	return err, err != nil && cw.n == 0
}

// ErrorStatus is the status of a response that carries errors and no data,
// where the media type lets the status say so. Such a response is usually the
// client's doing -- a document that did not parse or validate, a variable
// that did not coerce -- and that is 400. One carrying INTERNAL_SERVER_ERROR
// is not: a panic, or a policy backend that could not be reached, leaves no
// data either, and is the server's failure. Answered 400 it never reached
// 5xx alerting, and told a client not to retry.
func ErrorStatus(resp *graphql.Response) int {
	for _, e := range resp.Errors {
		if e.Extensions["code"] == graphql.CodeInternal {
			return http.StatusInternalServerError
		}
	}
	return http.StatusBadRequest
}

// WriteBatch writes n responses as one JSON array, calling entry for each. An
// entry that fails having written nothing -- a response that would not
// serialize -- is replaced by FallbackBody and the batch goes on: the array is
// already open by then, so stopping would leave the client `[{...},`, which is
// not JSON, and cost it the entries that did succeed. A failure after bytes
// went out is the client leaving, and ends the batch.
//
// warn is told about each entry replaced, since the client is not told why.
func WriteBatch(out io.Writer, n int, entry func(i int, w io.Writer) error, warn func(i int, err error)) error {
	if _, err := io.WriteString(out, "["); err != nil {
		return err
	}
	for i := range n {
		if i > 0 {
			if _, err := io.WriteString(out, ","); err != nil {
				return err
			}
		}
		err, wroteNothing := WriteBody(out, func(w io.Writer) error { return entry(i, w) })
		if err == nil {
			continue
		}
		if !wroteNothing {
			return err
		}
		warn(i, err)
		if _, err := io.WriteString(out, FallbackBody); err != nil {
			return err
		}
	}
	_, err := io.WriteString(out, "]")
	return err
}

// EventData returns a writer that keeps what is written through it inside one
// Server-Sent Event's data field: a line break in the payload starts another
// "data: " line instead of ending the field, and with it the event. A client
// joins the lines of one event with a newline, so the payload arrives whole,
// and a line break between JSON tokens is whitespace.
//
// The execution buffer holds compact JSON, but not everything in it is the
// engine's: a custom scalar writes its own bytes, and one built on
// json.Encoder ends them with a newline.
func EventData(w io.Writer) io.Writer { return &eventDataWriter{w: w} }

type eventDataWriter struct{ w io.Writer }

func (e *eventDataWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			n, err := e.w.Write(p)
			return written + n, err
		}
		if n, err := e.w.Write(p[:i]); err != nil {
			return written + n, err
		}
		if _, err := io.WriteString(e.w, "\ndata: "); err != nil {
			return written + i, err
		}
		// CRLF is one line break, not two.
		skip := 1
		if p[i] == '\r' && i+1 < len(p) && p[i+1] == '\n' {
			skip = 2
		}
		written += i + skip
		p = p[i+skip:]
	}
	return written, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Forgeable reports whether a browser could have sent the request
// cross-origin without a preflight: no Content-Type or one of the CORS
// "simple" types, and none of the preflight-forcing headers.
func Forgeable(src Source, headers []string) bool {
	for _, name := range headers {
		if src.Header(name) != "" {
			return false
		}
	}
	ct, _, err := mime.ParseMediaType(src.Header("Content-Type"))
	if err != nil {
		return true
	}
	switch ct {
	case "", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data":
		return true
	}
	return false
}

// ParseGET builds a request from the query parameters.
//
// queryOptional lets a request arrive without query text, which is only valid
// when something downstream can supply it -- automatic persisted queries send
// a hash alone. The caller must then check for itself that a query was found.
func ParseGET(src Source, queryOptional bool) (*graphql.Request, error) {
	req := &graphql.Request{Query: src.QueryParam("query"), OperationName: src.QueryParam("operationName")}
	if req.Query == "" && !queryOptional {
		return nil, ErrMissingQueryParam
	}
	if v := src.QueryParam("variables"); v != "" {
		if !IsJSONObject(v) {
			return nil, errors.New(`"variables" must be a JSON object`)
		}
		req.Variables = json.RawMessage(v)
	}
	if e := src.QueryParam("extensions"); e != "" {
		if err := json.Unmarshal([]byte(e), &req.Extensions); err != nil {
			return nil, fmt.Errorf(`"extensions" must be a JSON object: %w`, err)
		}
	}
	return req, nil
}

// RequireJSONBody rejects a POST whose Content-Type is not JSON.
func RequireJSONBody(src Source) error {
	ct, _, err := mime.ParseMediaType(src.Header("Content-Type"))
	if err != nil || ct != MediaTypeJSON {
		return fmt.Errorf("Content-Type must be %s.", MediaTypeJSON)
	}
	return nil
}

// ReadBody reads at most limit bytes. The returned status applies when err is
// non-nil, and distinguishes an over-long body from an unreadable one.
func ReadBody(src Source, limit int64) ([]byte, int, error) {
	body, err := src.Body(limit)
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes.", limit)
		}
		return nil, http.StatusBadRequest, fmt.Errorf("reading request body: %w", err)
	}
	return body, 0, nil
}

// Decode turns one JSON request object into a graphql.Request. See ParseGET
// for queryOptional.
//
// Leading JSON whitespace is skipped here rather than by each caller: gqlhttp
// and gqlfiber trimmed before calling, because they look for a batch array
// first, and the SSE handlers did not, so " {...}" was 200 over plain HTTP and
// 400 over SSE for the same body.
func Decode(body []byte, queryOptional bool) (*graphql.Request, error) {
	body = TrimJSONSpace(body)
	if len(body) == 0 {
		return nil, errors.New("request body is empty.")
	}
	if body[0] != '{' {
		return nil, errors.New("request body must be a JSON object.")
	}
	var req graphql.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if req.Query == "" && !queryOptional {
		return nil, ErrMissingQuery
	}
	if len(req.Variables) > 0 && !IsJSONObject(string(req.Variables)) {
		return nil, errors.New(`"variables" must be a JSON object or null.`)
	}
	return &req, nil
}

// TrimJSONSpace drops the leading whitespace RFC 8259 allows before a value:
// space, tab, line feed and carriage return, and nothing else.
func TrimJSONSpace(b []byte) []byte {
	return bytes.TrimLeft(b, " \t\r\n")
}

// IsJSONObject accepts an object or the literal null.
func IsJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	return s == "null" || (len(s) > 0 && s[0] == '{')
}
