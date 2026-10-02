// Package gqlmux serves GraphQL over HTTP, Server-Sent Events and WebSocket
// from one handler on one path.
//
//	mux.Handle("/graphql", gqlmux.New(exec))
//
// It adds no transport of its own: each request goes to gqlhttp, gqlsse or
// gqlws, chosen by what the request asks for. A WebSocket upgrade goes to
// gqlws. A request whose Accept header names text/event-stream, and prefers
// it to any JSON type it also names, goes to gqlsse. Everything else goes to
// gqlhttp.
//
// The reason to use it rather than the three handlers is not the path. An
// option that has to hold everywhere is given once: a safelist handed to two
// of three handlers leaves the third answering any query at all, and a drain
// that only one of the streaming handlers knows about waits out its deadline
// for the other's connections.
//
// It links all three transports, the WebSocket library among them: about half
// a megabyte and eight packages more than gqlhttp alone. A server that will
// never serve a subscription mounts gqlhttp and pays for nothing else.
package gqlmux

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/httpreq"
	"github.com/syssam/graphql-go/transport/drain"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// config collects each handler's options in two parts: what a shared option
// asked for, and what WithHTTP, WithSSE and WithWS asked for. The second is
// applied after the first, so a transport's own option overrides a shared one
// wherever the caller wrote it.
type config struct {
	http, ownHTTP []gqlhttp.Option
	sse, ownSSE   []gqlsse.Option
	ws, ownWS     []gqlws.Option
}

// Option configures the handler New returns.
type Option func(*config)

// WithPersistedQueries enables automatic persisted queries, or a safelist
// when cache is one (ext/trusted), on all three transports.
func WithPersistedQueries(cache apq.Cache) Option {
	return func(c *config) {
		c.http = append(c.http, gqlhttp.WithPersistedQueries(cache))
		c.sse = append(c.sse, gqlsse.WithPersistedQueries(cache))
		c.ws = append(c.ws, gqlws.WithPersistedQueries(cache))
	}
}

// WithDrain registers every SSE subscription stream and every WebSocket
// connection with d, so one d.Shutdown ends both. See transport/drain.
func WithDrain(d *drain.Drain) Option {
	return func(c *config) {
		c.sse = append(c.sse, gqlsse.WithDrain(d))
		c.ws = append(c.ws, gqlws.WithDrain(d))
	}
}

// WithLogger sets the logger for transport-level failures on all three
// transports. The default is slog.Default.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		c.http = append(c.http, gqlhttp.WithLogger(l))
		c.sse = append(c.sse, gqlsse.WithLogger(l))
		c.ws = append(c.ws, gqlws.WithLogger(l))
	}
}

// WithCSRFPrevention controls the check that rejects requests a browser could
// send cross-origin without a CORS preflight, on the two transports a plain
// request reaches. It is on by default; see gqlhttp.WithCSRFPrevention.
func WithCSRFPrevention(enabled bool, headers ...string) Option {
	return func(c *config) {
		c.http = append(c.http, gqlhttp.WithCSRFPrevention(enabled, headers...))
		c.sse = append(c.sse, gqlsse.WithCSRFPrevention(enabled, headers...))
	}
}

// WithMaxBodyBytes limits the size of request bodies over HTTP and SSE. The
// default is 1 MiB. A WebSocket message is bounded by gqlws.WithReadLimit.
func WithMaxBodyBytes(n int64) Option {
	return func(c *config) {
		c.http = append(c.http, gqlhttp.WithMaxBodyBytes(n))
		c.sse = append(c.sse, gqlsse.WithMaxBodyBytes(n))
	}
}

// WithWriteTimeout bounds how long one write to a stream or a WebSocket may
// take, which is what ends a peer that stays connected and stops reading. The
// default is 10s on both.
func WithWriteTimeout(d time.Duration) Option {
	return func(c *config) {
		c.sse = append(c.sse, gqlsse.WithWriteTimeout(d))
		c.ws = append(c.ws, gqlws.WithWriteTimeout(d))
	}
}

// WithHTTP passes options to the gqlhttp handler alone, such as
// gqlhttp.WithBatching. They are applied after the shared ones, so they win.
func WithHTTP(opts ...gqlhttp.Option) Option {
	return func(c *config) { c.ownHTTP = append(c.ownHTTP, opts...) }
}

// WithSSE passes options to the gqlsse handler alone, such as
// gqlsse.WithKeepAlive. They are applied after the shared ones, so they win.
func WithSSE(opts ...gqlsse.Option) Option {
	return func(c *config) { c.ownSSE = append(c.ownSSE, opts...) }
}

// WithWS passes options to the gqlws handler alone, such as
// gqlws.WithOnConnect. They are applied after the shared ones, so they win.
func WithWS(opts ...gqlws.Option) Option {
	return func(c *config) { c.ownWS = append(c.ownWS, opts...) }
}

// New returns one handler serving every operation kind over HTTP,
// Server-Sent Events and graphql-transport-ws.
func New(exec *graphql.Executor, opts ...Option) http.Handler {
	var c config
	for _, o := range opts {
		o(&c)
	}
	plain := gqlhttp.New(exec, append(c.http, c.ownHTTP...)...)
	stream := gqlsse.New(exec, append(c.sse, c.ownSSE...)...)
	socket := gqlws.New(exec, append(c.ws, c.ownWS...)...)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isWebSocket(r):
			socket.ServeHTTP(w, r)
		case httpreq.PrefersEventStream(r.Header.Get("Accept")):
			stream.ServeHTTP(w, r)
		default:
			plain.ServeHTTP(w, r)
		}
	})
}

// isWebSocket reports whether r asks to upgrade to a WebSocket. The header is
// a list of tokens, compared without case.
func isWebSocket(r *http.Request) bool {
	for _, v := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}
