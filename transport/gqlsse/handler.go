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
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/httpreq"
	"github.com/syssam/graphql-go/internal/jitter"
	"github.com/syssam/graphql-go/transport/drain"
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
	exec         *graphql.Executor
	maxBody      int64
	csrf         bool
	csrfHeaders  []string
	keepAlive    time.Duration
	maxStreamAge time.Duration
	writeTimeout time.Duration
	apq          apq.Cache
	logger       *slog.Logger
	drain        *drain.Drain
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

// WithMaxStreamAge ends a subscription stream after d, give or take 10% so
// that streams opened together do not all end together, without a complete
// event -- so the client reconnects rather than treating the subscription as
// finished for good. It does not apply to a single-result query or mutation,
// however long that takes. Zero
// means no limit, the default.
func WithMaxStreamAge(d time.Duration) Option { return func(h *Handler) { h.maxStreamAge = d } }

// WithWriteTimeout bounds how long one write to a stream may take before the
// stream is ended. The default is 10s; zero disables it.
//
// It is what ends a client that stays connected but stops reading, on every
// stream this handler opens: a subscription's, and the one next and complete
// that answer a query or mutation. The handler is then parked in a socket
// write, where it sees neither a drain nor its cancelled context, so without
// a deadline the stream outlives drain.Shutdown and http.Server.Shutdown
// both. A large payload is written in pieces with the deadline set again
// before each, so the bound is on a peer that takes nothing, not on how long
// a slow one needs for a large response.
//
// The deadline replaces http.Server.WriteTimeout for the stream, and is taken
// off between writes, so a server may set WriteTimeout for its other handlers
// without ending subscriptions at that age. With this option at zero nothing
// replaces it, and the server's WriteTimeout, being one deadline for the whole
// response, ends every stream when it passes. A ResponseWriter that cannot
// set a deadline -- one wrapped by middleware that does not expose Unwrap --
// is written without one.
func WithWriteTimeout(d time.Duration) Option { return func(h *Handler) { h.writeTimeout = d } }

// WithPersistedQueries enables automatic persisted queries backed by cache,
// for example apq.NewCache(1000). Disabled by default.
func WithPersistedQueries(cache apq.Cache) Option { return func(h *Handler) { h.apq = cache } }

// WithLogger sets the logger for transport-level failures such as write
// errors. The default is slog.Default.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.logger = l } }

// WithDrain registers every subscription stream with d, so d.Shutdown ends
// them without a complete event, which lets the client reconnect. Queries and
// mutations are ordinary requests that the server's own Shutdown waits for.
// Once d is draining, new subscriptions are refused with 503.
func WithDrain(d *drain.Drain) Option { return func(h *Handler) { h.drain = d } }

// New creates a handler streaming operations executed by exec.
func New(exec *graphql.Executor, opts ...Option) *Handler {
	h := &Handler{
		exec:        exec,
		maxBody:     1 << 20,
		csrf:        true,
		csrfHeaders: DefaultCSRFHeaders,
		keepAlive:   15 * time.Second,

		writeTimeout: 10 * time.Second,
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
	// Whether this is a stream or a 406 is decided by Accept, and behind
	// gqlmux so is whether the same URL answers with JSON instead.
	w.Header().Add("Vary", "Accept")
	if !httpreq.AcceptsEventStream(r.Header.Get("Accept")) {
		h.writeError(w, http.StatusNotAcceptable, "Accept header does not allow %s.", MediaTypeEventStream)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		h.writeError(w, http.StatusMethodNotAllowed, "Method %s is not allowed; use GET or POST.", r.Method)
		return
	}
	src := httpreq.FromRequest(w, r)

	var ct httpreq.ContentType
	if h.csrf && httpreq.Forgeable(src, &ct, h.csrfHeaders) {
		h.writeError(w, http.StatusForbidden,
			"This request could be forged cross-site. Send a non-simple Content-Type or one of the headers %s.", strings.Join(h.csrfHeaders, ", "))
		return
	}

	req, status, err := h.parse(src, &ct)
	if err != nil {
		h.writeError(w, status, "%v", err)
		return
	}

	// Resolution comes first: a request carrying only a hash has no query
	// text, so the checks below would have nothing to inspect.
	if h.apq != nil {
		if resp := apq.Resolve(h.apq, req); resp != nil {
			h.writeResponse(w, httpreq.PersistedStatus(resp), resp)
			return
		}
		if req.Query == "" {
			h.writeError(w, http.StatusBadRequest, "%v", httpreq.ErrMissingQuery)
			return
		}
	}

	// A mutation changes state, so it must not be reachable by a URL a
	// browser can be led to open. Unknown kinds fall through and surface as
	// a request error from the executor.
	if kind, kerr := h.exec.OperationKind(req.Query, req.OperationName); kerr == nil {
		if r.Method == http.MethodGet && kind == ast.Mutation {
			h.refuseMutationOverGET(w)
			return
		}
		if kind == ast.Subscription {
			h.subscribe(w, r, req)
			return
		}
	}
	h.single(w, r, req)
}

func (h *Handler) parse(src httpreq.Source, ct *httpreq.ContentType) (*graphql.Request, int, error) {
	if src.Method() == http.MethodGet {
		req, err := httpreq.ParseGET(src, h.apq != nil)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		return req, 0, nil
	}
	if err := httpreq.RequireJSONBody(src, ct); err != nil {
		return nil, http.StatusUnsupportedMediaType, err
	}
	body, status, err := httpreq.ReadBody(src, h.maxBody)
	if err != nil {
		return nil, status, err
	}
	req, err := httpreq.Decode(body, h.apq != nil)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return req, 0, nil
}

// single serves a query or mutation as one next event and a complete.
func (h *Handler) single(w http.ResponseWriter, r *http.Request, req *graphql.Request) {
	resp := h.exec.Execute(r.Context(), req)
	defer resp.Release()

	// The check in ServeHTTP read the text as received; this is the executor
	// refusing a mutation a RequestInterceptor put in its place.
	if req.QueryOnly && httpreq.QueryOnlyRefused(resp) {
		h.refuseMutationOverGET(w)
		return
	}
	// An operation that never ran is a transport-level failure, reported the
	// way gqlhttp reports it rather than as a payload on an opened stream.
	if resp.HasRequestErrors() {
		h.writeResponse(w, httpreq.ErrorStatus(resp), resp)
		return
	}

	// Under the write timeout like a subscription's events. This stream is
	// short, but a query's result can be the largest thing the handler ever
	// writes, and a client that asked for it and reads none of it held the
	// handler and the response buffer for as long as it stayed connected.
	rc := http.NewResponseController(w)
	out := h.timed(w, rc)
	out.Arm()
	h.beginStream(w)
	if err := h.writeNext(out, resp); err != nil {
		h.logWrite(err)
		return
	}
	if err := h.writeComplete(out); err != nil {
		h.logWrite(err)
		return
	}
	h.flushed(rc)
}

func (h *Handler) refuseMutationOverGET(w http.ResponseWriter) {
	h.writeError(w, http.StatusMethodNotAllowed, "%v", httpreq.ErrMutationOverGET)
}

// subscribe streams one next event per source event.
func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request, req *graphql.Request) {
	ctx, leave, ok := h.drain.Enter(r.Context())
	if !ok {
		h.writeError(w, http.StatusServiceUnavailable, "The server is shutting down.")
		return
	}
	defer leave()

	events, err := h.exec.Subscribe(ctx, req)
	if err != nil {
		var se *graphql.SubscribeError
		if errors.As(err, &se) {
			h.writeResponse(w, httpreq.ErrorStatus(se.Response), se.Response)
			return
		}
		// Subscribe returns nothing but a *SubscribeError, whose errors have
		// been through the executor's presenter. Anything else has not, so
		// its text is logged and not sent.
		h.logger.Warn("gqlsse: opening subscription", "error", err)
		h.writeResponse(w, http.StatusInternalServerError, httpreq.InternalError())
		return
	}

	rc := http.NewResponseController(w)
	out := h.timed(w, rc)
	out.Arm()
	h.beginStream(w)
	h.flushed(rc)

	var idle <-chan time.Time
	if h.keepAlive > 0 {
		t := time.NewTicker(h.keepAlive)
		defer t.Stop()
		idle = t.C
	}

	var aged <-chan time.Time
	if h.maxStreamAge > 0 {
		t := time.NewTimer(jitter.Spread(h.maxStreamAge))
		defer t.Stop()
		aged = t.C
	}

	for {
		select {
		case resp, ok := <-events:
			out.Arm()
			if !ok {
				if err := h.writeComplete(out); err != nil {
					h.logWrite(err)
					return
				}
				h.flushed(rc)
				return
			}
			err := h.writeNext(out, resp)
			resp.Release()
			if err != nil {
				// The client is gone. Returning cancels the request context,
				// which ends the executor's pump.
				h.logWrite(err)
				return
			}
			h.flushed(rc)
		case <-idle:
			out.Arm()
			if _, err := out.WriteString(":\n\n"); err != nil {
				h.logWrite(err)
				return
			}
			h.flushed(rc)
		case <-h.drain.Closing():
			// No complete: it would tell the client the subscription ended
			// for good, where ending the response makes it reconnect.
			return
		case <-aged:
			// No complete, as with a drain: the client should reconnect.
			return
		case <-ctx.Done():
			return
		}
	}
}

// timed returns the writer every event of a stream goes through: it sets the
// write deadline when told an event is starting and again for each further
// piece of a large one. The flush that follows an event is covered by the last
// deadline set, and takes it off.
func (h *Handler) timed(w io.Writer, rc *http.ResponseController) *httpreq.TimedWriter {
	return httpreq.NewTimedWriter(w, func() { h.armWrite(rc) }, nil)
}

// armWrite gives the event about to be written, and its flush, the write
// timeout. The deadline is per event rather than per stream: it is set before
// each one and taken off again by flush once the bytes are out.
func (h *Handler) armWrite(rc *http.ResponseController) {
	if h.writeTimeout > 0 {
		// A writer with no deadline to set is written without one.
		_ = rc.SetWriteDeadline(time.Now().Add(h.writeTimeout))
	}
}

// disarmWrite takes the deadline off between writes. Left in place it is a
// deadline on the stream, not on a write: HTTP/2 resets a stream whose write
// deadline passes whether or not anything is being written, so a subscription
// quiet for longer than the timeout was cut off, and on HTTP/1.1 the chunk
// that ends the response was written past it and failed.
func (h *Handler) disarmWrite(rc *http.ResponseController) {
	if h.writeTimeout > 0 {
		_ = rc.SetWriteDeadline(time.Time{})
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

// writeNext frames one response as the data of a next event. The payload is
// written through httpreq.EventData, so a line break inside it -- a custom
// scalar's, the engine writes none -- continues the data rather than ending
// the event.
func (h *Handler) writeNext(w io.Writer, resp *graphql.Response) error {
	if _, err := io.WriteString(w, "event: next\ndata: "); err != nil {
		return err
	}
	if err := h.writePayload(httpreq.EventData(w), resp); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

// writePayload writes resp, or httpreq.FallbackBody when resp failed to
// serialize before any of it went out, so an event whose extensions will not
// marshal still carries a parseable payload and the stream goes on. Only a
// failure after bytes were written -- the client leaving -- is returned.
func (h *Handler) writePayload(w io.Writer, resp *graphql.Response) error {
	err, wroteNothing := httpreq.WriteBody(w, func(out io.Writer) error {
		_, err := resp.WriteTo(out)
		return err
	})
	if err == nil || !wroteNothing {
		return err
	}
	h.logger.Warn("gqlsse: serializing response", "error", err)
	_, err = io.WriteString(w, httpreq.FallbackBody)
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

// flushed is flush for a write armWrite gave a deadline: the deadline covers
// the flush and comes off after it.
func (h *Handler) flushed(rc *http.ResponseController) {
	h.flush(rc)
	h.disarmWrite(rc)
}

func (h *Handler) logWrite(err error) {
	h.logger.Warn("gqlsse: writing event", "error", err)
}

// writeResponse sends a GraphQL response as an ordinary HTTP body, for
// failures raised before the stream opened.
func (h *Handler) writeResponse(w http.ResponseWriter, status int, resp *graphql.Response) {
	w.Header().Set("Content-Type", MediaTypeGraphQLResponse+"; charset=utf-8")
	w.WriteHeader(status)
	// The status is already on the wire, but the body is still empty after a
	// serialization failure, and an empty body reads as nothing having gone
	// wrong; see gqlhttp.writeResponse.
	if err := h.writePayload(w, resp); err != nil {
		h.logger.Warn("gqlsse: writing response", "error", err)
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, format string, args ...any) {
	h.writeResponse(w, status, &graphql.Response{Errors: []*graphql.Error{graphql.Errorf(format, args...)}})
}
