package gqlfiber

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// source adapts a Fiber request to the shared rule set, so that the CSRF
// check, the body limit and JSON decoding are the ones gqlhttp and gqlsse
// already apply rather than a second implementation.
type source struct{ c fiber.Ctx }

func newSource(c fiber.Ctx) httpreq.Source { return source{c: c} }

func (s source) Method() string            { return s.c.Method() }
func (s source) Header(name string) string { return s.c.Get(name) }

// QueryParam copies. Fiber returns query values as unsafe strings over the
// pooled fasthttp buffer, which another connection overwrites once the
// handler returns, but the query text reaches the plan cache and is kept for
// the process lifetime -- together with the parsed document's offsets into
// the same memory. Header and Body need no copy: their consumers decode them
// before the handler returns.
func (s source) QueryParam(name string) string { return strings.Clone(s.c.Query(name)) }

// Body enforces the limit as a length check: fasthttp has already read the
// whole body by the time a handler runs, so there is no reader left to cap.
//
// This is not identical to net/http's MaxBytesReader. Fiber's Body()
// decompresses a Content-Encoding body in full before returning it, so the
// limit applies to the decompressed size where gqlhttp caps wire bytes: an
// expansion bomb is materialised in memory before gqlfiber rejects it. What
// bounds the wire read is fiber.Config.BodyLimit, 4 MiB by default.
func (s source) Body(max int64) ([]byte, error) {
	b := s.c.Body()
	if int64(len(b)) > max {
		return nil, httpreq.ErrBodyTooLarge
	}
	return b, nil
}
