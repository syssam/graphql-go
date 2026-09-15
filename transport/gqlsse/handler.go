// Package gqlsse serves GraphQL over Server-Sent Events in the distinct
// connections mode of the graphql-sse protocol: one HTTP request per
// operation, results streamed back as "next" events and terminated by a
// "complete" event.
//
// All three operation types are served the same way, so one endpoint covers
// them: a query or mutation streams a single next followed by complete, a
// subscription streams one next per event until the source ends or the client
// disconnects. Request parsing, the body limit and the CSRF check are shared
// with gqlhttp, so the two transports accept and reject the same requests.
package gqlsse

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/internal/httpreq"
)

// MediaTypeEventStream is the response type of an open stream.
const MediaTypeEventStream = "text/event-stream"

// MediaTypeGraphQLResponse carries an error raised before the stream opens.
const MediaTypeGraphQLResponse = "application/graphql-response+json"

// DefaultCSRFHeaders are the headers whose presence marks a request as one
// that required a CORS preflight.
var DefaultCSRFHeaders = httpreq.DefaultCSRFHeaders

// Handler streams GraphQL operations over Server-Sent Events.
type Handler struct {
	exec        *graphql.Executor
	maxBody     int64
	csrf        bool
	csrfHeaders []string
	keepAlive   time.Duration
	logger      *slog.Logger
}

// Option configures a Handler.
type Option func(*Handler)

// WithMaxBodyBytes limits the size of request bodies. The default is 1 MiB.
func WithMaxBodyBytes(n int64) Option { return func(h *Handler) { h.maxBody = n } }

// WithCSRFPrevention controls the check that rejects requests a browser could
// send cross-origin without a CORS preflight. When enabled (the default),
// such requests must carry one of the given headers, or DefaultCSRFHeaders
// when none are given.
func WithCSRFPrevention(enabled bool, headers ...string) Option {
	return func(h *Handler) {
		h.csrf = enabled
		if len(headers) > 0 {
			h.csrfHeaders = headers
		}
	}
}

// WithKeepAlive sets how often a comment line is sent on an idle stream, so
// that proxies and load balancers do not treat a quiet subscription as a dead
// connection. The default is 15s; zero disables it.
func WithKeepAlive(d time.Duration) Option { return func(h *Handler) { h.keepAlive = d } }

// WithLogger sets the logger for transport-level failures such as write
// errors. The default is slog.Default.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.logger = l } }

// New creates a handler streaming operations executed by exec.
func New(exec *graphql.Executor, opts ...Option) *Handler {
	h := &Handler{
		exec:        exec,
		maxBody:     1 << 20,
		csrf:        true,
		csrfHeaders: DefaultCSRFHeaders,
		keepAlive:   15 * time.Second,
	}
	for _, o := range opts {
		o(h)
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !acceptsEventStream(r.Header.Get("Accept")) {
		h.writeError(w, http.StatusNotAcceptable, "Accept header does not allow %s.", MediaTypeEventStream)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		h.writeError(w, http.StatusMethodNotAllowed, "Method %s is not allowed; use GET or POST.", r.Method)
		return
	}
	if h.csrf && httpreq.Forgeable(r, h.csrfHeaders) {
		h.writeError(w, http.StatusForbidden,
			"This request could be forged cross-site. Send a non-simple Content-Type or one of the headers %s.", strings.Join(h.csrfHeaders, ", "))
		return
	}

	req, status, err := h.parse(w, r)
	if err != nil {
		h.writeError(w, status, "%v", err)
		return
	}

	// A mutation changes state, so it must not be reachable by a URL a
	// browser can be led to open. Unknown kinds fall through and surface as
	// a request error from the executor.
	if kind, kerr := h.exec.OperationKind(req.Query, req.OperationName); kerr == nil {
		if r.Method == http.MethodGet && kind == ast.Mutation {
			h.writeError(w, http.StatusMethodNotAllowed, "Mutations are not allowed over GET; use POST.")
			return
		}
		if kind == ast.Subscription {
			h.subscribe(w, r, req)
			return
		}
	}
	h.single(w, r, req)
}

func (h *Handler) parse(w http.ResponseWriter, r *http.Request) (*graphql.Request, int, error) {
	if r.Method == http.MethodGet {
		req, err := httpreq.ParseGET(r)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		return req, 0, nil
	}
	if err := httpreq.RequireJSONBody(r); err != nil {
		return nil, http.StatusUnsupportedMediaType, err
	}
	body, status, err := httpreq.ReadBody(w, r, h.maxBody)
	if err != nil {
		return nil, status, err
	}
	req, err := httpreq.Decode(body)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return req, 0, nil
}

// single serves a query or mutation as one next event and a complete.
func (h *Handler) single(w http.ResponseWriter, r *http.Request, req *graphql.Request) {
	resp := h.exec.Execute(r.Context(), req)
	defer resp.Release()

	// An operation that never ran is a transport-level failure, reported the
	// way gqlhttp reports it rather than as a payload on an opened stream.
	if resp.HasRequestErrors() {
		h.writeResponse(w, http.StatusBadRequest, resp)
		return
	}

	rc := http.NewResponseController(w)
	h.beginStream(w)
	if err := h.writeNext(w, resp); err != nil {
		h.logWrite(err)
		return
	}
	if err := h.writeComplete(w); err != nil {
		h.logWrite(err)
		return
	}
	h.flush(rc)
}

// subscribe streams one next event per source event.
func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request, req *graphql.Request) {
	ctx := r.Context()
	events, err := h.exec.Subscribe(ctx, req)
	if err != nil {
		var se *graphql.SubscribeError
		if errors.As(err, &se) {
			h.writeResponse(w, http.StatusBadRequest, se.Response)
			return
		}
		h.writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}

	rc := http.NewResponseController(w)
	h.beginStream(w)
	h.flush(rc)

	var idle <-chan time.Time
	if h.keepAlive > 0 {
		t := time.NewTicker(h.keepAlive)
		defer t.Stop()
		idle = t.C
	}

	for {
		select {
		case resp, ok := <-events:
			if !ok {
				if err := h.writeComplete(w); err != nil {
					h.logWrite(err)
					return
				}
				h.flush(rc)
				return
			}
			err := h.writeNext(w, resp)
			resp.Release()
			if err != nil {
				// The client is gone. Returning cancels the request context,
				// which ends the executor's pump.
				h.logWrite(err)
				return
			}
			h.flush(rc)
		case <-idle:
			if _, err := io.WriteString(w, ":\n\n"); err != nil {
				h.logWrite(err)
				return
			}
			h.flush(rc)
		case <-ctx.Done():
			return
		}
	}
}

func (h *Handler) beginStream(w http.ResponseWriter) {
	head := w.Header()
	head.Set("Content-Type", MediaTypeEventStream)
	// Proxies that buffer or transform the body would defeat streaming.
	head.Set("Cache-Control", "no-cache, no-transform")
	head.Set("Connection", "keep-alive")
	head.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeNext frames one response. Execution buffers hold compact JSON with no
// literal newline, so the payload always fits a single data line.
func (h *Handler) writeNext(w io.Writer, resp *graphql.Response) error {
	if _, err := io.WriteString(w, "event: next\ndata: "); err != nil {
		return err
	}
	if _, err := resp.WriteTo(w); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

func (h *Handler) writeComplete(w io.Writer) error {
	_, err := io.WriteString(w, "event: complete\ndata:\n\n")
	return err
}

func (h *Handler) flush(rc *http.ResponseController) {
	if err := rc.Flush(); err != nil {
		h.logger.Warn("gqlsse: flushing stream", "error", err)
	}
}

func (h *Handler) logWrite(err error) {
	h.logger.Warn("gqlsse: writing event", "error", err)
}

// writeResponse sends a GraphQL response as an ordinary HTTP body, for
// failures raised before the stream opened.
func (h *Handler) writeResponse(w http.ResponseWriter, status int, resp *graphql.Response) {
	w.Header().Set("Content-Type", MediaTypeGraphQLResponse+"; charset=utf-8")
	w.WriteHeader(status)
	if _, err := resp.WriteTo(w); err != nil {
		h.logger.Warn("gqlsse: writing response", "error", err)
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, format string, args ...any) {
	h.writeResponse(w, status, &graphql.Response{Errors: []*graphql.Error{graphql.Errorf(format, args...)}})
}

// acceptsEventStream reports whether the client will take a stream. An absent
// header is taken as yes so that command-line clients work; a header that
// names only other types is a client pointed at the wrong endpoint.
func acceptsEventStream(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mt, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		switch strings.TrimSpace(mt) {
		case MediaTypeEventStream, "text/*", "*/*":
			return true
		}
	}
	return false
}
