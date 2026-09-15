// Package gqlfiber serves GraphQL through Fiber v3.
//
// Fiber is built on fasthttp, not net/http, so this is a native
// implementation rather than a wrapper: requests are parsed through the rules
// in internal/httpreq and responses are written straight into the fasthttp
// response buffer. Routing a request through middleware/adaptor would rebuild
// a synthetic *http.Request per call and cannot carry a WebSocket at all.
package gqlfiber

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/gqlwsproto"
	"github.com/syssam/graphql-go/internal/httpreq"
)

// DefaultCSRFHeaders are the headers whose presence marks a request as one
// that required a CORS preflight. Any of them satisfies the CSRF check.
var DefaultCSRFHeaders = httpreq.DefaultCSRFHeaders

// ConnectFunc authenticates a WebSocket connection from its connection_init
// payload. The returned context is the parent of every operation on that
// connection, so a token decoded here is available to every resolver.
// Returning an error closes the connection with StatusForbidden.
type ConnectFunc = gqlwsproto.ConnectFunc

// config holds the settings of every constructor in this package.
type config struct {
	maxBody     int64
	batchMax    int
	csrf        bool
	csrfHeaders []string
	apq         apq.Cache

	keepAlive time.Duration

	initTimeout  time.Duration
	pingInterval time.Duration
	maxSubs      int
	readLimit    int64
	writeTimeout time.Duration
	onConnect    ConnectFunc

	originPatterns     []string
	insecureSkipOrigin bool

	logger *slog.Logger
}

// Option configures a constructor in this package.
//
// One type serves all of them, because Go allows only one WithLogger per
// package and splitting the rest by transport would buy nothing. An option a
// given constructor has no use for is ignored rather than rejected.
type Option func(*config)

// WithMaxBodyBytes limits the size of request bodies. The default is 1 MiB.
func WithMaxBodyBytes(n int64) Option { return func(c *config) { c.maxBody = n } }

// WithBatching accepts JSON arrays of requests with at most max entries,
// executed sequentially. Batching is disabled by default.
func WithBatching(max int) Option { return func(c *config) { c.batchMax = max } }

// WithCSRFPrevention controls the check that rejects requests a browser could
// send cross-origin without a CORS preflight. When enabled (the default),
// such requests must carry one of the given headers, or DefaultCSRFHeaders
// when none are given.
func WithCSRFPrevention(enabled bool, headers ...string) Option {
	return func(c *config) {
		c.csrf = enabled
		if len(headers) > 0 {
			c.csrfHeaders = headers
		}
	}
}

// WithPersistedQueries enables automatic persisted queries backed by cache,
// for example apq.NewCache(1000). Disabled by default.
func WithPersistedQueries(cache apq.Cache) Option { return func(c *config) { c.apq = cache } }

// WithKeepAlive sets how often a comment line is sent on an idle SSE stream,
// so that proxies and load balancers do not treat a quiet subscription as a
// dead connection. The default is 15s; zero disables it.
func WithKeepAlive(d time.Duration) Option { return func(c *config) { c.keepAlive = d } }

// WithInitTimeout bounds how long a WebSocket client may take to send
// connection_init before the connection is closed. The default is 10s.
func WithInitTimeout(d time.Duration) Option { return func(c *config) { c.initTimeout = d } }

// WithPingInterval sets how often the server sends a protocol ping on an idle
// WebSocket connection, so that a subscription producing nothing is
// distinguishable from a dead peer. The default is 20s; zero disables it.
func WithPingInterval(d time.Duration) Option { return func(c *config) { c.pingInterval = d } }

// WithMaxSubscriptions caps the operations one WebSocket connection may run
// at once. Beyond the cap a subscribe is answered with an error message and
// the connection stays open. The default is 100; zero means unlimited.
func WithMaxSubscriptions(n int) Option { return func(c *config) { c.maxSubs = n } }

// WithReadLimit caps the size of a single WebSocket client message. The
// default is 1 MiB.
func WithReadLimit(n int64) Option { return func(c *config) { c.readLimit = n } }

// WithWriteTimeout bounds how long one WebSocket write may take before the
// connection is torn down. The default is 10s.
//
// It exists because fasthttp/websocket accepts no context per message, so a
// deadline is the only bound available; coder/websocket, under the net/http
// transports, takes a context instead. It is deliberately not derived from
// WithPingInterval: a ping asks whether the peer is alive, a write deadline
// whether it is accepting bytes, and a peer can answer pings while its
// receive window stays full. Because gqlwsproto serializes writes, one peer
// stalled here blocks every subscription sharing its connection.
func WithWriteTimeout(d time.Duration) Option { return func(c *config) { c.writeTimeout = d } }

// WithOriginPatterns authorizes cross-origin WebSocket connections from hosts
// matching these patterns, which are matched with path.Match against the
// Origin's host, case-insensitively; a pattern containing "://" is matched
// against scheme and host instead. The request host is always authorized, so
// same-origin clients need no configuration.
func WithOriginPatterns(patterns ...string) Option {
	return func(c *config) { c.originPatterns = patterns }
}

// WithInsecureSkipOriginCheck disables origin verification for WebSocket
// upgrades. A browser can then open a connection from any site, carrying the
// user's cookies, with no CORS preflight in the way; prefer
// WithOriginPatterns.
func WithInsecureSkipOriginCheck() Option {
	return func(c *config) { c.insecureSkipOrigin = true }
}

// WithOnConnect registers the authentication hook run on connection_init.
func WithOnConnect(fn ConnectFunc) Option { return func(c *config) { c.onConnect = fn } }

// WithLogger sets the logger for transport-level failures such as write
// errors. The default is slog.Default.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// newConfig applies opts over the defaults shared by every constructor here,
// which are the defaults of gqlhttp, gqlsse and gqlws.
func newConfig(opts ...Option) *config {
	c := &config{
		maxBody:      1 << 20,
		csrf:         true,
		csrfHeaders:  DefaultCSRFHeaders,
		keepAlive:    15 * time.Second,
		initTimeout:  10 * time.Second,
		pingInterval: 20 * time.Second,
		maxSubs:      100,
		readLimit:    1 << 20,
		writeTimeout: 10 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// requestContext derives a cancellable context for one request.
//
// Fiber's Ctx documents itself as a context that can never be cancelled: its
// Done() is always nil. The executor uses cancellation for teardown --
// Subscribe's channel closes on it, and in-flight resolvers unwind through it
// -- so handing it Fiber's own context would leave a cancelled operation's
// goroutines with nothing to unwind them.
//
// What this cancels on is the handler returning, not the client
// disconnecting: Context() is context.Background() unless a middleware set
// one, so it carries no disconnect signal. A streaming transport learns the
// client left from its own read or write failing, and calls cancel then; this
// helper only guarantees that the operation is torn down by the time the
// handler is done.
//
// The parent is Context() rather than RequestCtx() so that values a
// middleware attached with SetContext reach the resolvers, and so that
// nothing holds a pooled *fasthttp.RequestCtx past the handler.
func requestContext(c fiber.Ctx) (context.Context, context.CancelFunc) {
	return context.WithCancel(c.Context())
}
