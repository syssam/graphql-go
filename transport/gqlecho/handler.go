package gqlecho

import (
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlsse"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// New returns a handler serving queries and mutations over HTTP. It accepts
// the same options as gqlhttp.New.
func New(exec *graphql.Executor, opts ...gqlhttp.Option) echo.HandlerFunc {
	h := gqlhttp.New(exec, opts...)
	return func(c *echo.Context) error { return serve(c, h) }
}

// SSE returns a handler streaming results over Server-Sent Events. It accepts
// the same options as gqlsse.New.
func SSE(exec *graphql.Executor, opts ...gqlsse.Option) echo.HandlerFunc {
	h := gqlsse.New(exec, opts...)
	return func(c *echo.Context) error { return serve(c, h) }
}

// WS returns a handler speaking graphql-transport-ws. It accepts the same
// options as gqlws.New.
//
// The upgrade hijacks the connection, so no status reaches the recorder and
// no HTTPError is ever raised: a refused upgrade has already written its own
// response.
func WS(exec *graphql.Executor, opts ...gqlws.Option) echo.HandlerFunc {
	h := gqlws.New(exec, opts...)
	return func(c *echo.Context) error {
		h.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}
