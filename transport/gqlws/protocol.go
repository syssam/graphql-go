// Package gqlws serves GraphQL over WebSocket using the graphql-transport-ws
// protocol: connection_init/connection_ack, ping/pong, and subscribe/next/
// error/complete per operation.
//
// All three operation types run over it. A query or mutation is a single next
// followed by complete, so a client can change an operation's kind without
// changing how it is sent.
package gqlws

import "encoding/json"

// Subprotocol is the WebSocket subprotocol this handler negotiates. A client
// that does not offer it is closed with StatusSubprotocolNotAcceptable.
const Subprotocol = "graphql-transport-ws"

// Protocol message types.
const (
	typeConnectionInit = "connection_init"
	typeConnectionAck  = "connection_ack"
	typePing           = "ping"
	typePong           = "pong"
	typeSubscribe      = "subscribe"
	typeNext           = "next"
	typeError          = "error"
	typeComplete       = "complete"
)

// Close codes defined by the protocol, beyond the RFC 6455 range.
const (
	StatusSubprotocolNotAcceptable = 4406
	StatusBadRequest               = 4400
	StatusUnauthorized             = 4401
	StatusForbidden                = 4403
	StatusInitTimeout              = 4408
	StatusSubscriberExists         = 4409
	StatusTooManyInitRequests      = 4429
)

// inMessage is a message from the client. Payload is left raw because its
// shape depends on Type.
type inMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// outMessage is a message to the client.
type outMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// subscribePayload is the payload of a subscribe message: a GraphQL request.
type subscribePayload struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName,omitempty"`
	Variables     json.RawMessage `json:"variables,omitempty"`
	Extensions    map[string]any  `json:"extensions,omitempty"`
}
