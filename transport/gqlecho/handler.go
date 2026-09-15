package gqlecho

import (
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

// New returns a handler serving queries and mutations over HTTP. It accepts
// the same options as gqlhttp.New.
func New(exec *graphql.Executor, opts ...gqlhttp.Option) echo.HandlerFunc {
	h := gqlhttp.New(exec, opts...)
	return func(c *echo.Context) error { return serve(c, h) }
}
