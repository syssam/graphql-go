package httpreq

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
)

// ErrMutationOverGET is how the SSE handlers refuse a mutation sent by GET.
// The plain-HTTP handlers word the same refusal with NotQueryOverGET; the two
// families differ on purpose, and each agrees with itself by reading its text
// from here.
var ErrMutationOverGET = errors.New("Mutations are not allowed over GET; use POST.")

// NotQueryOverGET is how the plain-HTTP handlers refuse a GET that named
// anything but a query.
func NotQueryOverGET(kind ast.Operation) error {
	return fmt.Errorf("%s operations (mutations are not allowed over GET); use POST.", strings.ToLower(string(kind)))
}

// InternalError is the body of a failure whose own text is not for a client.
// internal/gqlwsproto sends the same error and cannot import this package; a
// test there holds the two together.
func InternalError() *graphql.Response {
	return &graphql.Response{Errors: []*graphql.Error{
		graphql.Errorf("internal system error").WithCode(graphql.CodeInternal),
	}}
}

// PersistedStatus is the status an SSE handler gives a response apq.Resolve
// answered instead of letting the request run. A retry handshake is 200, as it
// is on every transport, because the client has to read it and send the text.
// Any other refusal -- a safelist's, a hash that does not match its text -- is
// a request error like the ones the executor raises, and those are 400 there:
// the body is application/graphql-response+json, the media type under which
// gqlhttp answers the same refusal 400.
func PersistedStatus(resp *graphql.Response) int {
	if apq.IsRetryHandshake(resp) {
		return http.StatusOK
	}
	return ErrorStatus(resp)
}
