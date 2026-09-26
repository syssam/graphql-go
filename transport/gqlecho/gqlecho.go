// Package gqlecho serves GraphQL through Echo v5.
//
// Echo is built on net/http, so these handlers delegate to the transports in
// transport/gqlhttp, transport/gqlsse and transport/gqlws rather than
// reimplementing anything. What they add over echo.WrapHandler is that
// failures occurring before a GraphQL response exists -- a rejected method,
// an unacceptable Accept header, a forgeable request -- reach Echo as an
// *echo.HTTPError, so an application's error handler and middleware see them.
//
// Every error status but 400 is raised this way, including the body limit's
// 413 and a drain's 503; 400 carries GraphQL request errors, which are a
// response rather than a failure (see reportable).
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

	if reportable(rec.status) {
		return echo.NewHTTPError(rec.status, http.StatusText(rec.status))
	}
	return nil
}

// reportable reports whether a status is a transport-level failure Echo's
// error handler should see: every error status but 400.
//
// It used to be an allow-list of four (403, 405, 406, 415), which let the
// body limit's 413, a drain's 503 and gqlsse's 500 past Echo's error
// middleware as if they had succeeded. Listing what is excluded instead means
// a status a handler starts emitting later is reported by default.
//
// 400 is excluded because it is the status of a GraphQL request error -- a
// document that fails validation under application/graphql-response+json,
// and every request error on the SSE handlers -- which is a GraphQL response
// the client asked for, not a transport failure. A malformed body is also
// 400, and the recorder sees only the status, so it cannot tell the two
// apart; reporting both would put every invalid query from every client in
// an application's error log.
func reportable(status int) bool {
	return status >= http.StatusBadRequest && status != http.StatusBadRequest
}
