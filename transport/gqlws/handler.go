package gqlws

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/internal/gqlwsproto"
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
	maxSubs      int
	readLimit    int64
	onConnect    ConnectFunc
	accept       websocket.AcceptOptions
	logger       *slog.Logger
}

// Option configures a Handler.
type Option func(*Handler)

// WithInitTimeout bounds how long a client may take to send connection_init
// before the connection is closed with StatusInitTimeout. The default is 10s.
func WithInitTimeout(d time.Duration) Option { return func(h *Handler) { h.initTimeout = d } }

// WithPingInterval sets how often the server sends a protocol ping on an idle
// connection, so that a subscription producing nothing is distinguishable
// from a dead peer. The default is 20s; zero disables it.
func WithPingInterval(d time.Duration) Option { return func(h *Handler) { h.pingInterval = d } }

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

// New creates a handler running operations with exec.
func New(exec *graphql.Executor, opts ...Option) *Handler {
	h := &Handler{
		exec:         exec,
		initTimeout:  10 * time.Second,
		pingInterval: 20 * time.Second,
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
	opts := h.accept
	ws, err := websocket.Accept(w, r, &opts)
	if err != nil {
		// Accept has already written a response.
		h.logger.Debug("gqlws: upgrade refused", "error", err)
		return
	}
	defer ws.CloseNow()

	if ws.Subprotocol() != Subprotocol {
		ws.Close(StatusSubprotocolNotAcceptable, "Subprotocol not acceptable")
		return
	}
	ws.SetReadLimit(h.readLimit)

	// The connection outlives the HTTP request once the handshake is done, and
	// coder/websocket documents the request context as unsafe to use past
	// Accept, so the connection gets a context of its own.
	gqlwsproto.Serve(context.Background(), coderSocket{ws: ws}, gqlwsproto.Config{
		Exec:         h.exec,
		InitTimeout:  h.initTimeout,
		PingInterval: h.pingInterval,
		MaxSubs:      h.maxSubs,
		OnConnect:    gqlwsproto.ConnectFunc(h.onConnect),
		DecorateContext: func(ctx context.Context) context.Context {
			return withRequest(ctx, r)
		},
		Logger: h.logger,
	})
}
