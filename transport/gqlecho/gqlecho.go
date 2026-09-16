// Package gqlecho serves GraphQL through Echo v5.
//
// Echo is built on net/http, so these handlers delegate to the transports in
// transport/gqlhttp, transport/gqlsse and transport/gqlws rather than
// reimplementing anything. What they add over echo.WrapHandler is that
// failures occurring before a GraphQL response exists -- a rejected method,
// an unacceptable Accept header, a forgeable request -- reach Echo as an
// *echo.HTTPError, so an application's error handler and middleware see them.
//
// That HTTPError is observational. The wrapped handler has already written
// the status and the response body by the time it is raised, so Echo's
// default error handler finds the response committed and returns without
// touching it. A replacement HTTPErrorHandler must do the same: one that
// calls c.JSON unconditionally logs "echo: response already written to
// client" and changes nothing a client sees. Use echo.UnwrapResponse on the
// Context's response writer and check Committed before writing.
//
// GraphQL errors are not translated: a response carrying field errors is a
// successful HTTP response, and the status rules for
// application/graphql-response+json belong to gqlhttp.
//
// Nothing here raises an HTTPError for a refused WebSocket upgrade, where
// gqlfiber raises a Fiber error: WS hands the request straight to gqlws
// rather than through the recorder below, because the upgrade hijacks the
// connection and a refused one has already written its own response. The
// difference is in what each framework's socket layer leaves to report, not a
// difference of policy.
package gqlecho

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// statusRecorder captures a transport-level status without buffering the
// body: only the status decides whether an HTTPError is raised, and the body
// of a successful response must stream straight through.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap gives http.ResponseController access to the underlying writer's
// Flusher and Hijacker, which an embedded interface does not promote. SSE
// flushing and the WebSocket upgrade both reach the real writer through it.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serve runs h and reports any transport-level failure as an *echo.HTTPError.
func serve(c *echo.Context, h http.Handler) error {
	rec := &statusRecorder{ResponseWriter: c.Response(), status: http.StatusOK}
	h.ServeHTTP(rec, c.Request())

	// These four are the statuses the handlers emit before a GraphQL response
	// exists. Everything else, 400 included, is a GraphQL response envelope.
	switch rec.status {
	case http.StatusMethodNotAllowed, http.StatusNotAcceptable,
		http.StatusForbidden, http.StatusUnsupportedMediaType:
		return echo.NewHTTPError(rec.status, http.StatusText(rec.status))
	}
	return nil
}
