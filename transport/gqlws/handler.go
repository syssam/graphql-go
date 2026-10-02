package gqlws

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/internal/gqlwsproto"
	"github.com/syssam/graphql-go/transport/drain"
)

// ConnectFunc authenticates a connection from its connection_init payload.
// The returned context is the parent of every operation on that connection,
// so a token decoded here is available to every resolver. Returning an error
// closes the connection with StatusForbidden.
type ConnectFunc = gqlwsproto.ConnectFunc

// Handler serves GraphQL operations over WebSocket.
type Handler struct {
	exec *graphql.Executor

	initTimeout  time.Duration
	pingInterval time.Duration
	writeTimeout time.Duration
	maxSubs      int
	readLimit    int64
	onConnect    ConnectFunc
	accept       websocket.AcceptOptions
	logger       *slog.Logger
	drain        *drain.Drain

	apq apq.Cache

	maxAge      time.Duration
	maxAgeGrace time.Duration
	maxIdle     time.Duration
}

// Option configures a Handler.
type Option func(*Handler)

// WithInitTimeout bounds how long a client may take to send connection_init
// before the connection is closed with StatusInitTimeout. The default is 10s;
// zero disables it.
func WithInitTimeout(d time.Duration) Option { return func(h *Handler) { h.initTimeout = d } }

// WithPingInterval sets how often the server sends a protocol ping on an idle
// connection, so that a subscription producing nothing is distinguishable
// from a dead peer. The default is 20s; zero disables it.
func WithPingInterval(d time.Duration) Option { return func(h *Handler) { h.pingInterval = d } }

// WithWriteTimeout bounds how long one message may take to write before the
// connection is torn down. The default is 10s; zero disables it.
//
// It is what ends a peer that stays connected but stops reading. The protocol
// serializes writes, so one write stalled on a full receive window holds every
// subscription on the connection behind it, and pings do not help: a peer can
// answer them while its receive window stays full. It is deliberately not
// derived from WithPingInterval for that reason.
func WithWriteTimeout(d time.Duration) Option { return func(h *Handler) { h.writeTimeout = d } }

// WithMaxSubscriptions caps the operations one connection may run at once.
// Beyond the cap a subscribe is answered with an error message and the
// connection stays open. The default is 100; zero means unlimited.
func WithMaxSubscriptions(n int) Option { return func(h *Handler) { h.maxSubs = n } }

// WithReadLimit caps the size of a single client message. The default is
// 1 MiB.
func WithReadLimit(n int64) Option { return func(h *Handler) { h.readLimit = n } }

// WithOnConnect registers the authentication hook run on connection_init.
func WithOnConnect(fn ConnectFunc) Option { return func(h *Handler) { h.onConnect = fn } }

// WithOriginPatterns authorizes cross-origin connections from hosts matching
// these patterns. The request host is always authorized, so same-origin
// clients need no configuration.
func WithOriginPatterns(patterns ...string) Option {
	return func(h *Handler) { h.accept.OriginPatterns = patterns }
}

// WithInsecureSkipOriginCheck disables origin verification entirely. A
// browser can then open a connection from any site, carrying the user's
// cookies; prefer WithOriginPatterns.
func WithInsecureSkipOriginCheck() Option {
	return func(h *Handler) { h.accept.InsecureSkipVerify = true }
}

// WithLogger sets the logger for connection-level failures. The default is
// slog.Default.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.logger = l } }

// WithPersistedQueries enables automatic persisted queries backed by cache,
// for example apq.NewCache(1000), on subscribe messages. Disabled by default.
// A miss reaches the client as an ordinary result carrying
// PersistedQueryNotFound, so it retries with the full query text.
func WithPersistedQueries(cache apq.Cache) Option { return func(h *Handler) { h.apq = cache } }

// resolvePersisted adapts the cache to the protocol's hook, and returns nil
// when no cache is configured so the protocol pays nothing for the feature.
func (h *Handler) resolvePersisted() func(*graphql.Request) *graphql.Response {
	if h.apq == nil {
		return nil
	}
	return func(r *graphql.Request) *graphql.Response { return apq.Resolve(h.apq, r) }
}

// WithDrain registers every connection with d, so d.Shutdown winds them down:
// subscriptions end, queries and mutations in flight finish, and the
// connection closes with StatusGoingAway. Once d is draining, new connections
// are refused with 503.
func WithDrain(d *drain.Drain) Option { return func(h *Handler) { h.drain = d } }

// WithMaxConnectionAge drains a connection once it has been open this long,
// give or take 10% so that connections opened together do not drain
// together: new operations are refused, subscriptions end without complete,
// queries and mutations in flight finish, and the connection closes with
// StatusGoingAway. It lets a load balancer spread long-lived connections
// again. grace, when positive, bounds how long the drain waits for those
// operations before closing anyway; zero waits for them. Zero age means no
// limit, the default. An operation sent while that drain waits gets a
// terminal error rather than a retry, which with a long-running query in
// flight happens on every rotation, not only at shutdown.
func WithMaxConnectionAge(age, grace time.Duration) Option {
	return func(h *Handler) { h.maxAge, h.maxAgeGrace = age, grace }
}

// WithMaxConnectionIdle closes a connection with StatusNormalClosure once it
// has had no operation in flight for this long, counted from the handshake
// or from the last operation ending. Subscriptions count as in flight; pings
// do not. Zero means no limit, the default.
func WithMaxConnectionIdle(d time.Duration) Option { return func(h *Handler) { h.maxIdle = d } }

// New creates a handler running operations with exec.
func New(exec *graphql.Executor, opts ...Option) *Handler {
	h := &Handler{
		exec:         exec,
		initTimeout:  10 * time.Second,
		pingInterval: 20 * time.Second,
		writeTimeout: 10 * time.Second,
		maxSubs:      100,
		readLimit:    1 << 20,
	}
	for _, o := range opts {
		o(h)
	}
	h.accept.Subprotocols = []string{Subprotocol}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

// ServeHTTP upgrades the request and serves the connection.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, leave, ok := h.drain.Enter(context.Background())
	if !ok {
		http.Error(w, "The server is shutting down.", http.StatusServiceUnavailable)
		return
	}
	defer leave()

	opts := h.accept
	ws, err := websocket.Accept(w, r, &opts)
	if err != nil {
		// Accept has already written a response.
		h.logger.Debug("gqlws: upgrade refused", "error", err)
		return
	}
	defer func() { _ = ws.CloseNow() }()

	if ws.Subprotocol() != Subprotocol {
		_ = ws.Close(StatusSubprotocolNotAcceptable, "Subprotocol not acceptable")
		return
	}
	ws.SetReadLimit(h.readLimit)

	// The connection outlives the HTTP request once the handshake is done, and
	// coder/websocket documents the request context as unsafe to use past
	// Accept, so the connection gets a context of its own -- from the drain,
	// not the request.
	gqlwsproto.Serve(ctx, coderSocket{ws: ws, writeTimeout: h.writeTimeout}, gqlwsproto.Config{
		Exec:                  h.exec,
		InitTimeout:           h.initTimeout,
		PingInterval:          h.pingInterval,
		MaxSubs:               h.maxSubs,
		OnConnect:             h.onConnect,
		Closing:               h.drain.Closing(),
		MaxConnectionAge:      h.maxAge,
		MaxConnectionAgeGrace: h.maxAgeGrace,
		MaxConnectionIdle:     h.maxIdle,
		ResolvePersisted:      h.resolvePersisted(),
		DecorateContext: func(ctx context.Context) context.Context {
			return withRequest(ctx, r)
		},
		ClearContext: withoutRequest,
		Logger:       h.logger,
	})
}
