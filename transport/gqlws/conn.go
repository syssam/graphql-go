package gqlws

import (
	"context"
	"time"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go/internal/gqlwsproto"
)

// coderSocket drives the protocol over coder/websocket.
type coderSocket struct {
	ws           *websocket.Conn
	writeTimeout time.Duration
}

func (s coderSocket) Read(ctx context.Context) ([]byte, error) {
	typ, data, err := s.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, gqlwsproto.ErrBinaryFrame
	}
	return data, nil
}

// Write bounds each write with its own deadline: the connection context has
// none. coder/websocket closes the connection when a write's context ends,
// which is what releases the read loop and every operation on it.
func (s coderSocket) Write(ctx context.Context, data []byte) error {
	if s.writeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.writeTimeout)
		defer cancel()
	}
	return s.ws.Write(ctx, websocket.MessageText, data)
}

func (s coderSocket) Close(code int, reason string) error {
	return s.ws.Close(websocket.StatusCode(code), reason)
}
