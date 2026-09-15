package gqlfiber

import (
	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// source adapts a Fiber request to the shared rule set, so that the CSRF
// check, the body limit and JSON decoding are the ones gqlhttp and gqlsse
// already apply rather than a second implementation.
type source struct{ c fiber.Ctx }

func newSource(c fiber.Ctx) httpreq.Source { return source{c: c} }

func (s source) Method() string                { return s.c.Method() }
func (s source) Header(name string) string     { return s.c.Get(name) }
func (s source) QueryParam(name string) string { return s.c.Query(name) }

// Body enforces the limit as a length check: fasthttp has already read the
// whole body by the time a handler runs, so there is no reader left to cap.
// fiber.Config.BodyLimit is the defence that stops the read; this keeps the
// client-visible result identical to net/http's MaxBytesReader.
func (s source) Body(max int64) ([]byte, error) {
	b := s.c.Body()
	if int64(len(b)) > max {
		return nil, httpreq.ErrBodyTooLarge
	}
	return b, nil
}
