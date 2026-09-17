package gqlwsproto

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/syssam/graphql-go"
)

// ErrBinaryFrame is what a driver returns from Read for a frame that is not
// text. The protocol has no way to report a malformed frame against an
// operation, so it ends the connection.
var ErrBinaryFrame = errors.New("gqlwsproto: binary frame")

// Socket is the transport under the protocol. Serve holds a lock across every
// Write, so Write need not be safe against concurrent Writes. Close, however,
// may be called while a Write is in flight — by the init timer, or by the read
// loop rejecting a message while operations are still streaming — and must
// serialize itself against one: a close frame interleaved with a data frame
// corrupts the stream.
//
// Read may ignore its context: gqlfiber's driver does, because
// fasthttp/websocket's ReadMessage cannot be interrupted, and a parked read
// there ends only when the connection does. So the protocol must never rely on
// cancelling a read to unblock anything — closing the socket is what it has —
// and a driver that can honour the context gains nothing the protocol depends
// on by doing so.
type Socket interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close(code int, reason string) error
}

// ConnectFunc authenticates a connection from its connection_init payload.
// The returned context is the parent of every operation on that connection,
// so a token decoded here is available to every resolver. Returning an error
// closes the connection with StatusForbidden.
type ConnectFunc func(ctx context.Context, initPayload []byte) (context.Context, error)

// Config is the protocol configuration a driver supplies.
type Config struct {
	Exec *graphql.Executor

	InitTimeout  time.Duration
	PingInterval time.Duration
	MaxSubs      int
	OnConnect    ConnectFunc

	// Closing, when closed, drains the connection: subscriptions end without
	// complete, queries and mutations in flight finish, new operations are
	// refused, and the connection then closes with StatusGoingAway. Optional.
	Closing <-chan struct{}

	// DecorateContext adds driver-specific values to the context a
	// ConnectFunc receives. gqlws uses it to carry the upgrade request,
	// which is where cookie auth arrives; a fasthttp driver carries its own
	// equivalent. Optional.
	DecorateContext func(context.Context) context.Context

	Logger *slog.Logger
}
