package gqlfiber

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/httpreq"
	"github.com/syssam/graphql-go/internal/jitter"
)

// MediaTypeEventStream is the response type of an open stream.
const MediaTypeEventStream = "text/event-stream"

// sseHandler streams GraphQL operations over Server-Sent Events.
type sseHandler struct {
	exec *graphql.Executor
	*config
}

// SSE returns a Fiber handler streaming results over Server-Sent Events in
// the distinct connections mode of the graphql-sse protocol, serving every
// operation kind the way gqlsse does: a query or mutation is one next event
// followed by complete, a subscription one next per event until the source
// ends or the client leaves.
//
// Fiber offers no disconnect signal, so a stream learns that its client is
// gone only from a write of its own failing. On a subscription with nothing
// to say that write is the keep-alive tick, which makes WithKeepAlive the
// interval at which a vanished client is noticed; with keep-alive disabled an
// idle subscription is held until its source ends on its own.
func SSE(exec *graphql.Executor, opts ...Option) fiber.Handler {
	h := &sseHandler{exec: exec, config: newConfig(opts...)}
	// Zero means disabled here as it does on gqlsse, rather than being
	// clamped to a floor: one option name must not mean two things across
	// transports. But on Fiber it costs the only liveness check there is, so
	// it is said out loud at start-up rather than left in a doc comment.
	if h.keepAlive <= 0 {
		h.logger.Warn("gqlfiber: SSE keep-alive is disabled; an idle subscription cannot notice a departed client")
	}
	return h.serve
}

// serve mirrors gqlsse.Handler.ServeHTTP: the same checks in the same order,
// producing the same statuses and messages. It always returns nil, because
// the response is already written.
func (h *sseHandler) serve(c fiber.Ctx) error {
	if !acceptsEventStream(c.Get("Accept")) {
		h.writeError(c, http.StatusNotAcceptable, "Accept header does not allow %s.", MediaTypeEventStream)
		return nil
	}

	method := c.Method()
	if method != http.MethodGet && method != http.MethodPost {
		c.Set("Allow", "GET, POST")
		h.writeError(c, http.StatusMethodNotAllowed, "Method %s is not allowed; use GET or POST.", method)
		return nil
	}

	src := newSource(c)

	if h.csrf && httpreq.Forgeable(src, h.csrfHeaders) {
		h.writeError(c, http.StatusForbidden,
			"This request could be forged cross-site. Send a non-simple Content-Type or one of the headers %s.", strings.Join(h.csrfHeaders, ", "))
		return nil
	}

	req, status, err := h.parse(src)
	if err != nil {
		h.writeError(c, status, "%v", err)
		return nil
	}

	// Resolution comes first: a request carrying only a hash has no query
	// text, so the checks below would have nothing to inspect.
	if h.apq != nil {
		if resp := apq.Resolve(h.apq, req); resp != nil {
			h.writeResponse(c, http.StatusOK, resp)
			return nil
		}
		if req.Query == "" {
			h.writeError(c, http.StatusBadRequest, "%v", httpreq.ErrMissingQuery)
			return nil
		}
	}

	// A mutation changes state, so it must not be reachable by a URL a
	// browser can be led to open. Unknown kinds fall through and surface as
	// a request error from the executor.
	if kind, kerr := h.exec.OperationKind(req.Query, req.OperationName); kerr == nil {
		if method == http.MethodGet && kind == ast.Mutation {
			h.writeError(c, http.StatusMethodNotAllowed, "Mutations are not allowed over GET; use POST.")
			return nil
		}
		if kind == ast.Subscription {
			return h.subscribe(c, req)
		}
	}
	return h.single(c, req)
}

func (h *sseHandler) parse(src httpreq.Source) (*graphql.Request, int, error) {
	if src.Method() == http.MethodGet {
		req, err := httpreq.ParseGET(src, h.apq != nil)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		return req, 0, nil
	}
	if err := httpreq.RequireJSONBody(src); err != nil {
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

// single serves a query or mutation as one next event and a complete. The
// result is complete before anything is written, so it needs no stream: the
// bytes are the ones gqlsse sends, buffered into the response in one go.
func (h *sseHandler) single(c fiber.Ctx, req *graphql.Request) error {
	ctx, cancel := requestContext(c)
	defer cancel()

	resp := h.exec.Execute(ctx, req)
	defer resp.Release()

	// An operation that never ran is a transport-level failure, reported the
	// way New's handler reports it rather than as a payload on an opened
	// stream.
	if resp.HasRequestErrors() {
		h.writeResponse(c, http.StatusBadRequest, resp)
		return nil
	}

	h.beginStream(c)
	if err := h.writeNext(c, resp); err != nil {
		h.logWrite(err)
		return nil
	}
	if err := h.writeComplete(c); err != nil {
		h.logWrite(err)
	}
	return nil
}

// subscribe streams one next event per source event.
func (h *sseHandler) subscribe(c fiber.Ctx, req *graphql.Request) error {
	reqCtx, cancel := requestContext(c)
	ctx, leave, ok := h.drain.Enter(reqCtx)
	if !ok {
		cancel()
		h.writeError(c, http.StatusServiceUnavailable, "The server is shutting down.")
		return nil
	}

	events, err := h.exec.Subscribe(ctx, req)
	if err != nil {
		leave()
		cancel()
		var se *graphql.SubscribeError
		if errors.As(err, &se) {
			h.writeResponse(c, http.StatusBadRequest, se.Response)
			return nil
		}
		h.writeError(c, http.StatusInternalServerError, "%v", err)
		return nil
	}

	h.beginStream(c)
	// fasthttp keeps the response head in its connection buffer until the
	// first body chunk, which on a quiet subscription may be minutes away.
	// An SSE client needs the 200 to know the stream is open.
	c.Response().ImmediateHeaderFlush = true

	// fasthttp starts the stream writer's goroutine inside SendStreamWriter,
	// not when this handler returns, so the closure runs alongside the rest
	// of the handler and goes on running after Fiber has recycled the Ctx.
	// Nothing below may capture c: it is not merely stale by then, it is
	// serving another request while this goroutine is still writing.
	//
	// Its error is discarded rather than returned, because by now the
	// response is committed to a stream: handing fiber an error would let
	// the application's error handler write a second body over it.
	_ = c.SendStreamWriter(func(w *bufio.Writer) {
		defer func() {
			// Leaving the stream writer is the end of the response however
			// it came about -- the source closed, or a flush found nobody
			// reading. Cancelling ends the executor's pump; draining hands
			// back the responses it had already built, and unblocks it if it
			// is mid-send into the unbuffered channel. gqlsse simply returns
			// and leaves those to the garbage collector.
			//
			// The drain cannot hang. The pump closes this channel on its way
			// out, and both points it can be parked at -- reading the
			// binding's channel and sending into this one -- select on the
			// operation context, so a subscription source that ignores its
			// own context is not what decides whether this ends.
			cancel()
			for resp := range events {
				resp.Release()
			}
			// Last, so a drain waits for this stream's cleanup and not only
			// for its writes.
			leave()
		}()
		defer func() {
			// fasthttp runs this closure on a bare goroutine of its own with
			// no recover, so a panic here would end the process. Under
			// net/http the same panic costs gqlsse one connection, and a
			// panic three transports survive must not kill the fourth.
			if r := recover(); r != nil {
				h.logger.Error("gqlfiber: panic while streaming",
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		h.stream(ctx, w, events)
	})
	return nil
}

// stream pumps events onto an open stream until the source ends, the context
// is cancelled, or a write shows that nobody is reading.
func (h *sseHandler) stream(ctx context.Context, w *bufio.Writer, events <-chan *graphql.Response) {
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
			if !ok {
				if err := h.writeComplete(w); err != nil {
					h.logWrite(err)
					return
				}
				h.flush(w)
				return
			}
			err := h.writeNext(w, resp)
			resp.Release()
			if err != nil {
				h.logWrite(err)
				return
			}
			if !h.flush(w) {
				return
			}
		case <-idle:
			if _, err := w.WriteString(":\n\n"); err != nil {
				h.logWrite(err)
				return
			}
			if !h.flush(w) {
				return
			}
		case <-h.drain.Closing():
			// No complete event, as in gqlsse: the client should reconnect.
			return
		case <-aged:
			// No complete, as with a drain: the client should reconnect.
			return
		case <-ctx.Done():
			return
		}
	}
}

// beginStream sets the response head of an open stream.
func (h *sseHandler) beginStream(c fiber.Ctx) {
	c.Set("Content-Type", MediaTypeEventStream)
	// Proxies that buffer or transform the body would defeat streaming.
	c.Set("Cache-Control", "no-cache, no-transform")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
}

// writeNext frames one response. Execution buffers hold compact JSON with no
// literal newline, so the payload always fits a single data line.
func (h *sseHandler) writeNext(w io.Writer, resp *graphql.Response) error {
	if _, err := io.WriteString(w, "event: next\ndata: "); err != nil {
		return err
	}
	if _, err := resp.WriteTo(w); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

func (h *sseHandler) writeComplete(w io.Writer) error {
	_, err := io.WriteString(w, "event: complete\ndata:\n\n")
	return err
}

// flush pushes buffered bytes towards the client and reports whether the
// stream is still worth writing to.
//
// A failed flush is the only disconnect signal fasthttp offers: fiber.Ctx can
// never be cancelled, and the request context is context.Background. So this
// result is what eventually cancels the operation, and on a stream with
// nothing to say it is the keep-alive tick that produces it -- which is why
// disabling keep-alive leaves an idle subscription unable to notice that its
// client has gone.
func (h *sseHandler) flush(w *bufio.Writer) bool {
	if err := w.Flush(); err != nil {
		h.logger.Warn("gqlfiber: flushing stream", "error", err)
		return false
	}
	return true
}

func (h *sseHandler) logWrite(err error) {
	h.logger.Warn("gqlfiber: writing event", "error", err)
}

// writeResponse sends a GraphQL response as an ordinary HTTP body, for
// failures raised before the stream opened.
func (h *sseHandler) writeResponse(c fiber.Ctx, status int, resp *graphql.Response) {
	c.Set("Content-Type", MediaTypeGraphQLResponse+"; charset=utf-8")
	c.Status(status)
	if _, err := resp.WriteTo(c); err != nil {
		h.logger.Warn("gqlfiber: writing response", "error", err)
	}
}

func (h *sseHandler) writeError(c fiber.Ctx, status int, format string, args ...any) {
	h.writeResponse(c, status, &graphql.Response{Errors: []*graphql.Error{graphql.Errorf(format, args...)}})
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
