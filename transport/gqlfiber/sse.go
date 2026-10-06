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
	// As in gqlsse: stream or 406 is decided by Accept.
	c.Vary("Accept")
	if !httpreq.AcceptsEventStream(c.Get("Accept")) {
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

	var ct httpreq.ContentType
	if h.csrf && httpreq.Forgeable(src, &ct, h.csrfHeaders) {
		h.writeError(c, http.StatusForbidden,
			"This request could be forged cross-site. Send a non-simple Content-Type or one of the headers %s.", strings.Join(h.csrfHeaders, ", "))
		return nil
	}

	req, status, err := h.parse(src, &ct)
	if err != nil {
		h.writeError(c, status, "%v", err)
		return nil
	}

	// Resolution comes first: a request carrying only a hash has no query
	// text, so the checks below would have nothing to inspect.
	if h.apq != nil {
		if resp := apq.Resolve(h.apq, req); resp != nil {
			h.writeResponse(c, httpreq.PersistedStatus(resp), resp)
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
			h.refuseMutationOverGET(c)
			return nil
		}
		if kind == ast.Subscription {
			return h.subscribe(c, req)
		}
	}
	return h.single(c, req)
}

func (h *sseHandler) parse(src httpreq.Source, ct *httpreq.ContentType) (*graphql.Request, int, error) {
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
//
// The result is complete before anything is written, and under a write
// timeout it is still sent through a stream writer rather than buffered into
// the response. Buffered, it is fasthttp that writes it, after this handler
// has returned, under whatever fiber.Config.WriteTimeout says -- nothing, by
// default -- so a client that asked for a large result and read none of it
// held the connection and the response for as long as it stayed. A stream
// writer is where this package can put its own deadline on the connection, a
// piece at a time, as it does for a subscription.
func (h *sseHandler) single(c fiber.Ctx, req *graphql.Request) error {
	ctx, cancel := requestContext(c)
	defer cancel()

	resp := h.exec.Execute(ctx, req)

	// The check in serve read the text as received; this is the executor
	// refusing a mutation a RequestInterceptor put in its place.
	if req.QueryOnly && httpreq.QueryOnlyRefused(resp) {
		resp.Release()
		h.refuseMutationOverGET(c)
		return nil
	}
	// An operation that never ran is a transport-level failure, reported the
	// way New's handler reports it rather than as a payload on an opened
	// stream.
	if resp.HasRequestErrors() {
		h.writeResponse(c, httpreq.ErrorStatus(resp), resp)
		resp.Release()
		return nil
	}

	h.beginStream(c)
	if h.writeTimeout <= 0 {
		// No deadline to set, so nothing a stream writer is for: it costs a
		// goroutine and a pipe to hand fasthttp the same bytes. They differ
		// only in framing, a Content-Length here and chunks there.
		defer resp.Release()
		if err := h.writeNext(c, resp); err != nil {
			h.logWrite(err)
			return nil
		}
		if err := h.writeComplete(c); err != nil {
			h.logWrite(err)
		}
		return nil
	}
	arm := h.armer(c)
	// The closure outlives this handler and must not capture c; see subscribe.
	_ = c.SendStreamWriter(func(w *bufio.Writer) {
		// The response is released here, not by the handler: its buffer is
		// what the closure is writing.
		defer resp.Release()
		defer h.recoverStream()
		out := httpreq.NewTimedWriter(w, arm, w.Flush)
		out.Arm()
		if err := h.writeNext(out, resp); err != nil {
			h.logWrite(err)
			return
		}
		if err := h.writeComplete(out); err != nil {
			h.logWrite(err)
			return
		}
		arm()
		h.flush(w)
		// For what fasthttp still has to write once this returns; see
		// subscribe.
		arm()
	})
	return nil
}

func (h *sseHandler) refuseMutationOverGET(c fiber.Ctx) {
	h.writeError(c, http.StatusMethodNotAllowed, "%v", httpreq.ErrMutationOverGET)
}

// armer returns the function that gives the next write on this response's
// connection the write timeout. It takes the connection now because it is
// called from a stream writer, after c has been recycled: the connection is
// the one thing that still belongs to this response then.
func (h *sseHandler) armer(c fiber.Ctx) func() {
	conn := c.RequestCtx().Conn()
	return func() {
		if h.writeTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(h.writeTimeout))
		}
	}
}

// recoverStream is deferred in a stream writer. fasthttp runs one on a bare
// goroutine of its own with no recover, so a panic there would end the
// process. Under net/http the same panic costs gqlsse one connection, and a
// panic three transports survive must not kill the fourth.
func (h *sseHandler) recoverStream() {
	if r := recover(); r != nil {
		h.logger.Error("gqlfiber: panic while streaming",
			"panic", r, "stack", string(debug.Stack()))
	}
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
			h.writeResponse(c, httpreq.ErrorStatus(se.Response), se.Response)
			return nil
		}
		// Subscribe returns nothing but a *SubscribeError, whose errors have
		// been through the executor's presenter. Anything else has not, so
		// its text is logged and not sent.
		h.logger.Warn("gqlfiber: opening subscription", "error", err)
		h.writeResponse(c, http.StatusInternalServerError, httpreq.InternalError())
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
	// The connection is the exception, and is taken here for that reason:
	// it belongs to this response until the stream writer returns. It is what
	// carries the write deadline. A subscriber that stays connected and stops
	// reading blocks fasthttp's write to the socket, then the pipe this
	// closure writes into, then the closure -- in a Flush, where it sees
	// neither the drain nor its context, so the stream outlived both.
	arm := h.armer(c)

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
		defer h.recoverStream()
		h.stream(ctx, w, events, arm)
		// However the stream ended, fasthttp still has what is in the pipe
		// and the chunk that terminates the response to write, to a peer that
		// may not be reading. The deadline of the last event is no bound on
		// that: after a quiet spell it has already passed, and the response
		// was cut short. A fresh one is, and it can stay armed, because the
		// connection is closed after this response (beginStream).
		arm()
	})
	return nil
}

// stream pumps events onto an open stream until the source ends, the context
// is cancelled, or a write shows that nobody is reading. arm gives a write the
// write timeout.
//
// Every write goes through a TimedWriter, which sets the connection's
// deadline for each event and again for each further piece of a large one,
// flushing the piece on to fasthttp first. The pipe between the two holds a
// few pieces and then blocks, so a client that takes nothing stops the writer
// within a few pieces, and the last deadline set is the one its connection
// fails on.
func (h *sseHandler) stream(ctx context.Context, w *bufio.Writer, events <-chan *graphql.Response, arm func()) {
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

	out := httpreq.NewTimedWriter(w, arm, w.Flush)
	for {
		select {
		case resp, ok := <-events:
			out.Arm()
			if !ok {
				if err := h.writeComplete(out); err != nil {
					h.logWrite(err)
					return
				}
				h.flush(w)
				return
			}
			err := h.writeNext(out, resp)
			resp.Release()
			if err != nil {
				h.logWrite(err)
				return
			}
			if !h.flush(w) {
				return
			}
		case <-idle:
			out.Arm()
			if _, err := out.WriteString(":\n\n"); err != nil {
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
	// Closed after the stream rather than kept alive. The write deadline that
	// bounds each event lives on the connection, and fasthttp does not reset
	// it between responses unless a WriteTimeout is configured, so a
	// connection handed back for reuse would carry the stream's last deadline
	// into whatever response came next.
	c.Response().SetConnectionClose()
	c.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
}

// writeNext frames one response as the data of a next event, as gqlsse's
// does: through httpreq.EventData, so a line break a custom scalar wrote
// continues the data rather than ending the event.
func (h *sseHandler) writeNext(w io.Writer, resp *graphql.Response) error {
	if _, err := io.WriteString(w, "event: next\ndata: "); err != nil {
		return err
	}
	if err := h.writePayload(httpreq.EventData(w), resp); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

// writePayload is gqlsse's: resp, or httpreq.FallbackBody when resp failed to
// serialize before any of it went out. Only a failure after bytes were
// written -- the client leaving -- is returned.
func (h *sseHandler) writePayload(w io.Writer, resp *graphql.Response) error {
	err, wroteNothing := httpreq.WriteBody(w, func(out io.Writer) error {
		_, err := resp.WriteTo(out)
		return err
	})
	if err == nil || !wroteNothing {
		return err
	}
	h.logger.Warn("gqlfiber: serializing response", "error", err)
	_, err = io.WriteString(w, httpreq.FallbackBody)
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
	// See gqlhttp.writeResponse: an empty body reads as success.
	if err := h.writePayload(c, resp); err != nil {
		h.logger.Warn("gqlfiber: writing response", "error", err)
	}
}

func (h *sseHandler) writeError(c fiber.Ctx, status int, format string, args ...any) {
	h.writeResponse(c, status, &graphql.Response{Errors: []*graphql.Error{graphql.Errorf(format, args...)}})
}
