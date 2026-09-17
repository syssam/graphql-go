package gqlfiber

import (
	"context"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	// The v3 module. github.com/gofiber/contrib/websocket -- the path most
	// documentation and search results point at -- is the Fiber v2 module: its
	// go.mod pins fiber/v2 and it does not build against v3. The wrong path
	// reads like the right one, so do not "normalise" it.
	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/internal/gqlwsproto"
)

// Subprotocol is the WebSocket subprotocol WS negotiates. A client that does
// not offer it is closed with StatusSubprotocolNotAcceptable.
const Subprotocol = gqlwsproto.Subprotocol

// Close codes defined by the protocol, beyond the RFC 6455 range.
const (
	StatusSubprotocolNotAcceptable = gqlwsproto.StatusSubprotocolNotAcceptable
	StatusBadRequest               = gqlwsproto.StatusBadRequest
	StatusUnauthorized             = gqlwsproto.StatusUnauthorized
	StatusForbidden                = gqlwsproto.StatusForbidden
	StatusInitTimeout              = gqlwsproto.StatusInitTimeout
	StatusSubscriberExists         = gqlwsproto.StatusSubscriberExists
	StatusTooManyInitRequests      = gqlwsproto.StatusTooManyInitRequests
	StatusGoingAway                = gqlwsproto.StatusGoingAway
)

type connKey struct{}

// UpgradeConn is what a ConnectFunc can read of the request that opened the
// connection: its headers, cookies, query arguments, route parameters and
// locals, copied out before fasthttp recycled the request, plus the client
// address.
//
// It is an interface of our own rather than the upgrader's concrete
// connection type so that the WebSocket library stays an implementation
// detail. Widening this interface later is additive; changing a concrete
// return type would break every caller.
type UpgradeConn interface {
	Headers(key string, defaultValue ...string) string
	Cookies(key string, defaultValue ...string) string
	Query(key string, defaultValue ...string) string
	Params(key string, defaultValue ...string) string
	Locals(key string, value ...any) any
	IP() string
}

// ConnFrom returns the connection a ConnectFunc is authenticating, or nil.
//
// It is the Fiber counterpart of gqlws.RequestFrom: a browser cannot set
// headers on a WebSocket, so token auth arrives in the connection_init
// payload while cookie auth arrives on the upgrade request. Only the context
// a ConnectFunc receives carries it; the connection's later operations do
// not, because reading the socket from a resolver would race the protocol.
func ConnFrom(ctx context.Context) UpgradeConn {
	c, _ := ctx.Value(connKey{}).(UpgradeConn)
	return c
}

// fastSocket drives the protocol over fasthttp/websocket.
//
// The library is a gorilla derivative, which differs from coder/websocket in
// three ways that matter here. It has no per-message context, so cancellation
// is replaced by deadlines; it does not serialize writes, which is why
// gqlwsproto holds a lock across every Write; and it leaves the connection
// open after a failed write, where coder/websocket tears it down.
type fastSocket struct {
	conn         *websocket.Conn
	writeTimeout time.Duration

	// mu serializes Close against an in-flight Write. gqlwsproto guarantees
	// only that Writes do not overlap each other: Close may arrive from the
	// init timer or from the read loop while operations are still streaming,
	// and a close frame interleaved with a data frame corrupts the stream.
	mu     sync.Mutex
	closed bool
}

// Read blocks until a frame arrives or the connection errors. ctx is ignored
// because ReadMessage cannot be interrupted; see WS for what that costs.
func (s *fastSocket) Read(_ context.Context) ([]byte, error) {
	typ, data, err := s.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if typ != websocket.TextMessage {
		return nil, gqlwsproto.ErrBinaryFrame
	}
	return data, nil
}

func (s *fastSocket) Write(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		return err
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		// Nothing else would ever end this connection: the peer is not
		// reading, so it will not send either, and the read loop is parked in
		// ReadMessage where no context reaches it. coder/websocket closes on a
		// failed write for the same reason.
		//
		// closed means "the socket is gone", not "Close ran", so a later
		// protocol close does not set a deadline on a dead connection and try
		// to frame a close message into it.
		s.closed = true
		_ = s.conn.Close()
		return err
	}
	return nil
}

func (s *fastSocket) Close(code int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	err := s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
	if cerr := s.conn.Close(); err == nil {
		err = cerr
	}
	return err
}

// WS returns a Fiber handler speaking graphql-transport-ws, serving every
// operation kind: a query or mutation is one next followed by complete, a
// subscription one next per event.
//
// # Origins
//
// Cross-origin connections are refused unless WithOriginPatterns authorizes
// them; the request host is always authorized, so same-origin clients need no
// configuration. A request with no Origin header is accepted, because it is
// not a browser -- and it is browsers the check defends against: a page on
// any site can open a WebSocket to this handler carrying the user's cookies,
// and no CORS preflight stands in the way. WithInsecureSkipOriginCheck turns
// the check off.
//
// # Deadlines
//
// fasthttp/websocket takes no context per message, so a peer that stops
// reading is bounded by a write deadline instead: see WithWriteTimeout.
// Reads have no such bound, deliberately. A read deadline would have to be
// derived from the protocol ping interval, which would close a client that
// answers pings late or not at all; the consequence is that a connection
// whose peer vanished without closing its socket is held until the next
// server write fails, which on an idle connection is the next ping.
func WS(exec *graphql.Executor, opts ...Option) fiber.Handler {
	cfg := newConfig(opts...)
	warnBadOriginPatterns(cfg)

	upgrade := websocket.New(func(conn *websocket.Conn) {
		sock := &fastSocket{conn: conn, writeTimeout: cfg.writeTimeout}
		// The socket is hijacked out of fasthttp with KeepHijackedConns set,
		// so nothing upstream ever closes it: the library closes only when the
		// handler panics, and the protocol calls Close only for the failures
		// it names -- a client that simply goes away takes none of those
		// paths. By the time Serve returns it has waited out every writer, so
		// this is the last word on the descriptor.
		defer func() { _ = conn.Close() }()

		// Checked again here because the check below the upgrade can race a
		// drain that starts in between; the socket is open by now, so the
		// refusal is a close rather than a status.
		ctx, leave, ok := cfg.drain.Enter(context.Background())
		if !ok {
			_ = sock.Close(StatusGoingAway, "Going away")
			return
		}
		defer leave()

		if conn.Subprotocol() != Subprotocol {
			_ = sock.Close(StatusSubprotocolNotAcceptable, "Subprotocol not acceptable")
			return
		}
		conn.SetReadLimit(cfg.readLimit)

		// The connection is hijacked: this runs after the Fiber handler has
		// returned and its Ctx has been recycled, so the connection gets a
		// context of its own rather than anything derived from the request.
		gqlwsproto.Serve(ctx, sock, gqlwsproto.Config{
			Exec:         exec,
			InitTimeout:  cfg.initTimeout,
			PingInterval: cfg.pingInterval,
			MaxSubs:      cfg.maxSubs,
			OnConnect:    cfg.onConnect,
			Closing:      cfg.drain.Closing(),
			DecorateContext: func(ctx context.Context) context.Context {
				return context.WithValue(ctx, connKey{}, conn)
			},
			Logger: cfg.logger,
		})
	}, websocket.Config{
		Subprotocols: []string{Subprotocol},
		// The upgrader's own Origins list cannot express "the request host is
		// always authorized", nor patterns, so the check below replaces it
		// rather than layering under it. Saying so explicitly keeps an empty
		// list from reading as an oversight.
		Origins:          []string{"*"},
		AllowEmptyOrigin: true,
		HandshakeTimeout: cfg.writeTimeout,
	})

	return func(c fiber.Ctx) error {
		if !websocket.IsWebSocketUpgrade(c) {
			return fiber.ErrUpgradeRequired
		}
		if !originAllowed(c, cfg) {
			return fiber.NewError(fiber.StatusForbidden, "Origin not authorized")
		}
		select {
		case <-cfg.drain.Closing():
			return fiber.NewError(fiber.StatusServiceUnavailable, "The server is shutting down.")
		default:
		}
		return upgrade(c)
	}
}

// warnBadOriginPatterns reports a pattern that can never match. Patterns are
// known at construction, and a silent deny would leave a typo looking exactly
// like a correctly configured same-origin-only handler -- the kind of
// security misconfiguration that survives to production because nothing ever
// said anything.
func warnBadOriginPatterns(cfg *config) {
	for _, pattern := range cfg.originPatterns {
		if _, err := path.Match(strings.ToLower(pattern), ""); err != nil {
			cfg.logger.Warn("gqlfiber: WebSocket origin pattern is malformed and will never match",
				"pattern", pattern, "error", err)
		}
	}
}

// originAllowed mirrors coder/websocket's check, so that the two WebSocket
// transports authorize the same connections from the same configuration.
func originAllowed(c fiber.Ctx, cfg *config) bool {
	if cfg.insecureSkipOrigin {
		return true
	}
	origin := c.Get(fiber.HeaderOrigin)
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Host honours a forwarded host only behind a trusted proxy, so an
	// attacker cannot nominate the host its own Origin will be compared to.
	if strings.EqualFold(u.Host, c.Host()) {
		return true
	}
	for _, pattern := range cfg.originPatterns {
		target := u.Host
		if strings.Contains(pattern, "://") {
			target = u.Scheme + "://" + u.Host
		}
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target))
		if err == nil && matched {
			return true
		}
	}
	return false
}
