// Package httpreq holds the request handling the HTTP transports share:
// query-parameter and JSON-body decoding, the body limit and the CSRF check.
//
// It exists so that gqlhttp and gqlsse cannot drift apart on rules a client
// can tell the difference between. A request rejected as forgeable by one
// transport must be rejected by the other.
package httpreq

import (
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
var DefaultCSRFHeaders = []string{"GraphQL-Require-Preflight", "X-Requested-With"}

// Forgeable reports whether a browser could have sent r cross-origin without
// a preflight: no Content-Type or one of the CORS "simple" types, and none
// of the preflight-forcing headers.
func Forgeable(r *http.Request, headers []string) bool {
	for _, name := range headers {
		if r.Header.Get(name) != "" {
			return false
		}
	}
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
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
func ParseGET(r *http.Request, queryOptional bool) (*graphql.Request, error) {
	q := r.URL.Query()
	req := &graphql.Request{Query: q.Get("query"), OperationName: q.Get("operationName")}
	if req.Query == "" && !queryOptional {
		return nil, ErrMissingQueryParam
	}
	if v := q.Get("variables"); v != "" {
		if !IsJSONObject(v) {
			return nil, errors.New(`"variables" must be a JSON object`)
		}
		req.Variables = json.RawMessage(v)
	}
	if e := q.Get("extensions"); e != "" {
		if err := json.Unmarshal([]byte(e), &req.Extensions); err != nil {
			return nil, fmt.Errorf(`"extensions" must be a JSON object: %v`, err)
		}
	}
	return req, nil
}

// RequireJSONBody rejects a POST whose Content-Type is not JSON.
func RequireJSONBody(r *http.Request) error {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != MediaTypeJSON {
		return fmt.Errorf("Content-Type must be %s.", MediaTypeJSON)
	}
	return nil
}

// ReadBody reads at most max bytes. The returned status applies when err is
// non-nil, and distinguishes an over-long body from an unreadable one.
func ReadBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes.", max)
		}
		return nil, http.StatusBadRequest, fmt.Errorf("reading request body: %v", err)
	}
	return body, 0, nil
}

// Decode turns one JSON request object into a graphql.Request. See ParseGET
// for queryOptional.
func Decode(body []byte, queryOptional bool) (*graphql.Request, error) {
	if len(body) == 0 {
		return nil, errors.New("request body is empty.")
	}
	if body[0] != '{' {
		return nil, errors.New("request body must be a JSON object.")
	}
	var req graphql.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	if req.Query == "" && !queryOptional {
		return nil, ErrMissingQuery
	}
	if len(req.Variables) > 0 && !IsJSONObject(string(req.Variables)) {
		return nil, errors.New(`"variables" must be a JSON object or null.`)
	}
	return &req, nil
}

// IsJSONObject accepts an object or the literal null.
func IsJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	return s == "null" || (len(s) > 0 && s[0] == '{')
}
