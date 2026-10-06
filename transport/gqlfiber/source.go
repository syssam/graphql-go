package gqlfiber

import (
	"bytes"

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

// Repeated counts the header in the request as it arrived. fasthttp keeps one
// Content-Type, the last, so nothing it parsed can say there were two.
func (s source) Repeated(name string) bool {
	raw := s.c.Request().Header.RawHeaders()
	want := []byte(name)
	seen := false
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			raw = nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 || !bytes.EqualFold(bytes.TrimSpace(line[:colon]), want) {
			continue
		}
		if seen {
			return true
		}
		seen = true
	}
	return false
}

// QueryParam copies, and parses as net/http does. The query string lives in
// the pooled fasthttp buffer, which another connection overwrites once the
// handler returns, but the query text reaches the plan cache and is kept for
// the process lifetime -- together with the parsed document's offsets into
// the same memory; converting to a string is the copy. Header and Body need
// none: their consumers decode them before the handler returns.
func (s source) QueryParam(name string) string {
	return httpreq.QueryValue(string(s.c.Request().URI().QueryString()), name)
}

// Body enforces the limit as a length check: fasthttp has already read the
// whole body by the time a handler runs, so there is no reader left to cap.
// What bounds the wire read is fiber.Config.BodyLimit, 4 MiB by default.
//
// It is the body as sent. Fiber's Body() decompresses by Content-Encoding,
// which the net/http handlers never do -- compression is the surrounding
// server's business -- so the same request had two answers, and a decode that
// failed came back as Fiber's error text standing in for the body, with
// Fiber's status already written to the response.
func (s source) Body(limit int64) ([]byte, error) {
	b := s.c.BodyRaw()
	if int64(len(b)) > limit {
		return nil, httpreq.ErrBodyTooLarge
	}
	return b, nil
}
