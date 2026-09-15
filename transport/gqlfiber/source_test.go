package gqlfiber

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"unsafe"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/internal/httpreq"
)

// capture runs one request through a Fiber app and hands the handler's Source
// to fn.
func capture(t *testing.T, app *fiber.App, method, target, body string, fn func(httpreq.Source)) {
	t.Helper()
	app.Add([]string{method}, "/graphql", func(c fiber.Ctx) error {
		fn(newSource(c))
		return nil
	})
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

func TestSourceReadsTheRequest(t *testing.T) {
	capture(t, fiber.New(), "POST", "/graphql?query=%7Ba%7D", `{"query":"{a}"}`, func(src httpreq.Source) {
		if got := src.Method(); got != "POST" {
			t.Errorf("Method() = %q, want POST", got)
		}
		if got := src.Header("Content-Type"); got != "application/json" {
			t.Errorf("Header() = %q", got)
		}
		if got := src.QueryParam("query"); got != "{a}" {
			t.Errorf("QueryParam() = %q, want {a}", got)
		}
		body, err := src.Body(1 << 20)
		if err != nil {
			t.Fatalf("Body() error = %v", err)
		}
		if string(body) != `{"query":"{a}"}` {
			t.Errorf("Body() = %q", body)
		}
	})
}

// fiber.Ctx.Query hands back an unsafe string over the pooled fasthttp
// buffer, which fasthttp recycles onto another connection once the handler
// returns -- but ParseGET's result reaches the plan cache, which keeps the
// query text and the parsed document's position offsets for the process
// lifetime. Aliasing that buffer is an unsynchronised read of bytes another
// goroutine is writing.
//
// Pool reuse cannot be provoked from app.Test, which builds a fresh
// RequestCtx per call, so this asserts the absence of aliasing directly.
func TestSourceQueryParamDoesNotAliasThePooledBuffer(t *testing.T) {
	capture(t, fiber.New(), "GET", "/graphql?query=%7Ba%7D", "", func(src httpreq.Source) {
		got := src.QueryParam("query")
		if got != "{a}" {
			t.Errorf("QueryParam() = %q, want {a}", got)
			return
		}
		raw := src.(source).c.RequestCtx().QueryArgs().Peek("query")
		if len(raw) == 0 {
			t.Error("QueryArgs().Peek is empty; the test cannot compare addresses")
			return
		}
		if unsafe.StringData(got) == &raw[0] {
			t.Error("QueryParam() aliases the pooled fasthttp buffer; the plan cache outlives it")
		}
	})
}

// Fiber reads bodies eagerly, so the limit is a length check rather than a
// capped reader. The error must still be the one ReadBody turns into a 413.
func TestSourceBodyTooLarge(t *testing.T) {
	capture(t, fiber.New(), "POST", "/graphql", strings.Repeat("x", 100), func(src httpreq.Source) {
		if _, err := src.Body(10); !errors.Is(err, httpreq.ErrBodyTooLarge) {
			t.Fatalf("Body() error = %v, want ErrBodyTooLarge", err)
		}
	})
}
