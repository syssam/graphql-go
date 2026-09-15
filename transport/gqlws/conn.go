package gqlws

import (
	"context"

	"github.com/coder/websocket"

	"github.com/syssam/graphql-go/internal/gqlwsproto"
)

// coderSocket drives the protocol over coder/websocket.
type coderSocket struct{ ws *websocket.Conn }

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

func (s coderSocket) Write(ctx context.Context, data []byte) error {
	return s.ws.Write(ctx, websocket.MessageText, data)
}

func (s coderSocket) Close(code int, reason string) error {
	return s.ws.Close(websocket.StatusCode(code), reason)
}
