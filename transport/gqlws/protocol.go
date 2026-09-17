// Package gqlws serves GraphQL over WebSocket using the graphql-transport-ws
// protocol: connection_init/connection_ack, ping/pong, and subscribe/next/
// error/complete per operation.
//
// All three operation types run over it. A query or mutation is a single next
// followed by complete, so a client can change an operation's kind without
// changing how it is sent.
package gqlws

import "github.com/syssam/graphql-go/internal/gqlwsproto"

// Subprotocol is the WebSocket subprotocol this handler negotiates. A client
// that does not offer it is closed with StatusSubprotocolNotAcceptable.
const Subprotocol = gqlwsproto.Subprotocol

// Close codes defined by the protocol, beyond the RFC 6455 range.
const (
	StatusGoingAway                = gqlwsproto.StatusGoingAway
	StatusSubprotocolNotAcceptable = gqlwsproto.StatusSubprotocolNotAcceptable
	StatusBadRequest               = gqlwsproto.StatusBadRequest
	StatusUnauthorized             = gqlwsproto.StatusUnauthorized
	StatusForbidden                = gqlwsproto.StatusForbidden
	StatusInitTimeout              = gqlwsproto.StatusInitTimeout
	StatusSubscriberExists         = gqlwsproto.StatusSubscriberExists
	StatusTooManyInitRequests      = gqlwsproto.StatusTooManyInitRequests
)
