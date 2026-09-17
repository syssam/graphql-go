// Package gqlwsproto implements the graphql-transport-ws protocol —
// connection_init/connection_ack, ping/pong, and subscribe/next/error/complete
// per operation — independently of any WebSocket library.
//
// A driver owns the handshake, origin checks and framing, and hands the
// protocol a Socket; the state machine below is shared by every driver so that
// a protocol bug has one place to live.
//
// All three operation types run over it. A query or mutation is a single next
// followed by complete, so a client can change an operation's kind without
// changing how it is sent.
package gqlwsproto

import "encoding/json"

// Subprotocol is the WebSocket subprotocol this protocol negotiates. A client
// that does not offer it is closed with StatusSubprotocolNotAcceptable.
const Subprotocol = "graphql-transport-ws"

// Protocol message types.
const (
	TypeConnectionInit = "connection_init"
	TypeConnectionAck  = "connection_ack"
	TypePing           = "ping"
	TypePong           = "pong"
	TypeSubscribe      = "subscribe"
	TypeNext           = "next"
	TypeError          = "error"
	TypeComplete       = "complete"
)

// Close codes defined by the protocol, beyond the RFC 6455 range.
const (
	// StatusGoingAway is RFC 6455's 1001, sent when the server is shutting
	// down. graphql-ws clients treat it as retryable.
	StatusGoingAway                = 1001
	StatusSubprotocolNotAcceptable = 4406
	StatusBadRequest               = 4400
	StatusUnauthorized             = 4401
	StatusForbidden                = 4403
	StatusInitTimeout              = 4408
	StatusSubscriberExists         = 4409
	StatusTooManyInitRequests      = 4429
)

// InMessage is a message from the client. Payload is left raw because its
// shape depends on Type.
type InMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// OutMessage is a message to the client.
type OutMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SubscribePayload is the payload of a subscribe message: a GraphQL request.
type SubscribePayload struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName,omitempty"`
	Variables     json.RawMessage `json:"variables,omitempty"`
	Extensions    map[string]any  `json:"extensions,omitempty"`
}
