package httpreq

import (
	"errors"
	"io"
	"net/http"
	"net/url"
)

// ErrBodyTooLarge reports a body over the configured limit. Transports turn
// it into 413; the limit is enforced differently per transport but the
// client-visible result must not differ.
var ErrBodyTooLarge = errors.New("request body too large")

// Source is the request surface the transports share.
//
// It exists so that a transport built on something other than net/http --
// fasthttp, today -- reaches the same CSRF, body-limit and decoding rules
// rather than reimplementing them. A rule enforced here is enforced
// everywhere.
type Source interface {
	Method() string
	Header(name string) string
	// Repeated reports whether the request carried the header more than once.
	// Header answers with one value however many there were, and net/http
	// picks the first where fasthttp keeps the last, so a rule that reads a
	// repeated header would have two answers.
	Repeated(name string) bool
	QueryParam(name string) string
	Body(limit int64) ([]byte, error)
}

// netHTTP adapts a net/http request. The ResponseWriter is held because
// MaxBytesReader needs it to mark the connection unusable after an
// over-long body.
type netHTTP struct {
	w http.ResponseWriter
	r *http.Request
}

// FromRequest adapts a net/http request to a Source.
func FromRequest(w http.ResponseWriter, r *http.Request) Source { return netHTTP{w: w, r: r} }

func (s netHTTP) Method() string                { return s.r.Method }
func (s netHTTP) Header(name string) string     { return s.r.Header.Get(name) }
func (s netHTTP) Repeated(name string) bool     { return len(s.r.Header.Values(name)) > 1 }
func (s netHTTP) QueryParam(name string) string { return QueryValue(s.r.URL.RawQuery, name) }

// QueryValue is the first value of name in a raw query string, by net/url's
// rules: a pair that does not unescape is dropped, so a parameter with invalid
// percent-encoding is absent rather than passed on half decoded. Every Source
// reads its parameters through it. fasthttp decodes leniently, and a GET that
// net/http refused for a missing query reached the parser on Fiber.
func QueryValue(rawQuery, name string) string {
	values, _ := url.ParseQuery(rawQuery)
	return values.Get(name)
}

func (s netHTTP) Body(limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(s.w, s.r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, ErrBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}
