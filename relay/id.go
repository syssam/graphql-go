package relay

import (
	"encoding/base64"
	"errors"
	"strings"

	graphql "github.com/syssam/graphql-go"
)

// ErrInvalidGlobalID is returned by FromGlobalID for an id this server did
// not issue. It is deliberately one error for every malformed shape: telling
// a client which part failed tells it how to probe.
var ErrInvalidGlobalID = errors.New("relay: invalid global id")

// ToGlobalID encodes a type name and a type-local id into the opaque id a
// Relay client stores and hands back to Query.node.
//
// The encoding is base64("Type:id") with standard padding, which is what
// graphql-relay-js and graphql-java produce, so clients written against
// either interoperate.
func ToGlobalID(typeName, id string) graphql.ID {
	return graphql.ID(base64.StdEncoding.EncodeToString([]byte(typeName + ":" + id)))
}

// FromGlobalID reverses ToGlobalID. Only the first colon separates the two
// halves, so a local id may contain colons of its own.
func FromGlobalID(gid graphql.ID) (typeName, id string, err error) {
	raw, err := base64.StdEncoding.DecodeString(string(gid))
	if err != nil {
		return "", "", ErrInvalidGlobalID
	}
	typeName, id, found := strings.Cut(string(raw), ":")
	if !found || typeName == "" || id == "" {
		return "", "", ErrInvalidGlobalID
	}
	return typeName, id, nil
}
