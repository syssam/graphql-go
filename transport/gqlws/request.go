package gqlws

import (
	"context"
	"net/http"
)

type requestKey struct{}

func withRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

// withoutRequest hides the request from everything derived from ctx: a nil
// under the same key shadows the value withRequest put there.
func withoutRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestKey{}, (*http.Request)(nil))
}

// RequestFrom returns the HTTP request that opened the connection, or nil.
//
// It is available inside a ConnectFunc, where cookies and headers are often
// where the credential actually is: a browser cannot set headers on a
// WebSocket, so token auth arrives in the connection_init payload while
// cookie auth arrives on the upgrade request. Only the context a ConnectFunc
// receives carries it; the connection's later operations do not, because the
// request is finished by then and reading it would be a data race.
func RequestFrom(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}
