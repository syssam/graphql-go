package httpreq

import (
	"errors"
	"io"
	"net/http"
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
	QueryParam(name string) string
	Body(max int64) ([]byte, error)
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
func (s netHTTP) QueryParam(name string) string { return s.r.URL.Query().Get(name) }

func (s netHTTP) Body(max int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(s.w, s.r.Body, max))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, ErrBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}
