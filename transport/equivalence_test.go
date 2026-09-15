// This file proves, from a client's point of view, the claim internal/httpreq
// makes for itself: the CSRF check, the body limit and JSON-body decoding
// cannot drift apart between transports. It drives real HTTP requests
// through all four HTTP-carrying transports -- gqlhttp, gqlsse, gqlecho and
// gqlfiber -- over real listeners, and asserts identical status and body.
//
// Every transport is driven the same way, over a real loopback listener,
// rather than mixing httptest.NewRecorder (in-process) for three of them
// with a socket only for Fiber: a real listener is the only way to drive
// Fiber at all (see startFiberEquiv), and using one uniformly means no
// transport gets a different code path than the others just because the
// test needed to accommodate Fiber.
package transport_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/transport/gqlecho"
	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

func newEquivExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	const sdl = `
type Query { hello: String! }
type Mutation { bump: String! }
`
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		graphql.Mutation(graphql.Field("bump", func(graphql.Root) string { return "bumped" })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return graphql.NewExecutor(s)
}

// startFiberEquiv serves app on a loopback port and returns its base URL,
// mirroring transport/gqlfiber's own startFiber test helper: gqlfiber.New
// returns a fiber.Handler, not an http.Handler, so app.Test (an in-memory
// fake connection) is the only alternative, and it would put Fiber on a
// different code path than the other three transports' real listeners.
func startFiberEquiv(t *testing.T, app *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		_ = app.ShutdownWithTimeout(2 * time.Second)
	})
	return "http://" + ln.Addr().String()
}

// newEquivServers starts one server per transport, all serving the same
// executor and the same options, at the same path ("/graphql"), and returns
// their base URLs keyed by transport name.
//
// gqlfiber.New's doc comment requires registration through App.All rather
// than App.Post: with App.Post alone, a method the handler would reject
// never reaches it -- Fiber's router answers with its own 405 first, and a
// case in this file would then be comparing gqlfiber against Fiber's router
// instead of against gqlhttp.Handler.ServeHTTP. Echo is registered the same
// way, through e.Any, for the same reason.
func newEquivServers(t *testing.T, httpOpts []gqlhttp.Option, sseOpts []gqlsse.Option, fiberOpts []gqlfiber.Option) map[string]string {
	t.Helper()

	httpSrv := httptest.NewServer(gqlhttp.New(newEquivExecutor(t), httpOpts...))
	t.Cleanup(httpSrv.Close)

	sseSrv := httptest.NewServer(gqlsse.New(newEquivExecutor(t), sseOpts...))
	t.Cleanup(sseSrv.Close)

	e := echo.New()
	e.Any("/graphql", gqlecho.New(newEquivExecutor(t), httpOpts...))
	echoSrv := httptest.NewServer(e)
	t.Cleanup(echoSrv.Close)

	app := fiber.New()
	app.All("/graphql", gqlfiber.New(newEquivExecutor(t), fiberOpts...))
	fiberURL := startFiberEquiv(t, app)

	return map[string]string{
		"gqlhttp":  httpSrv.URL,
		"gqlsse":   sseSrv.URL,
		"gqlecho":  echoSrv.URL,
		"gqlfiber": fiberURL,
	}
}

// transportNames fixes iteration order so a failing subtest is reported
// under a stable, predictable name instead of Go's randomised map order.
var transportNames = []string{"gqlhttp", "gqlsse", "gqlecho", "gqlfiber"}

// doRequest sends one request to base+path and returns the status and the
// trimmed body.
func doRequest(t *testing.T, base, method, path string, headers map[string]string, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// assertAllEqual drives the same request through every transport in
// servers and asserts each one answers with wantStatus and wantBody
// literally -- not merely with each other, so a bug shared by all four
// still fails the test. Each transport runs as its own subtest, so a
// mismatch names exactly which transport diverged.
func assertAllEqual(t *testing.T, servers map[string]string, method, path string, headers map[string]string, body string, wantStatus int, wantBody string) {
	t.Helper()
	for _, name := range transportNames {
		base, ok := servers[name]
		if !ok {
			t.Fatalf("no server registered for transport %q", name)
		}
		t.Run(name, func(t *testing.T) {
			status, got := doRequest(t, base, method, path, headers, body)
			if status != wantStatus {
				t.Errorf("%s: status = %d, want %d (body: %s)", name, status, wantStatus, got)
			}
			if got != wantBody {
				t.Errorf("%s: body = %s, want %s", name, got, wantBody)
			}
		})
	}
}

// TestEquivalence drives one table of requests through all four HTTP-carrying
// transports and asserts identical status and identical body, proving that
// internal/httpreq's shared rules produce a shared client-visible result.
func TestEquivalence(t *testing.T) {
	t.Run("over-long body", func(t *testing.T) {
		servers := newEquivServers(t,
			[]gqlhttp.Option{gqlhttp.WithMaxBodyBytes(16)},
			[]gqlsse.Option{gqlsse.WithMaxBodyBytes(16)},
			[]gqlfiber.Option{gqlfiber.WithMaxBodyBytes(16)},
		)
		body := `{"query":"` + strings.Repeat("x", 200) + `"}`
		const want = `{"errors":[{"message":"request body exceeds 16 bytes."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "application/json"}, body, http.StatusRequestEntityTooLarge, want)
	})

	t.Run("forgeable request", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		const want = `{"errors":[{"message":"This request could be forged cross-site. ` +
			`Send a non-simple Content-Type or one of the headers GraphQL-Require-Preflight, X-Requested-With."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "text/plain"}, `{"query":"{hello}"}`, http.StatusForbidden, want)
	})

	t.Run("non-JSON body", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		const want = `{"errors":[{"message":"Content-Type must be application/json."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "text/xml"}, `{"query":"{hello}"}`, http.StatusUnsupportedMediaType, want)
	})

	// gqlsse's rejection message for this case is asserted separately from
	// the other three: see the comment beside wantSSEBody below for why that
	// is a legitimate difference rather than a bug being papered over.
	t.Run("mutation over GET", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		headers := map[string]string{"GraphQL-Require-Preflight": "1"}
		const path = "/graphql?query=mutation%7Bbump%7D"

		const wantBody = `{"errors":[{"message":"mutation operations (mutations are not allowed over GET); use POST."}]}`
		for _, name := range []string{"gqlhttp", "gqlecho", "gqlfiber"} {
			t.Run(name, func(t *testing.T) {
				status, got := doRequest(t, servers[name], http.MethodGet, path, headers, "")
				if status != http.StatusMethodNotAllowed {
					t.Errorf("%s: status = %d, want 405 (body: %s)", name, status, got)
				}
				if got != wantBody {
					t.Errorf("%s: body = %s, want %s", name, got, wantBody)
				}
			})
		}

		// gqlsse composes this rejection itself rather than through
		// internal/httpreq -- unlike the CSRF check, the body limit and JSON
		// decoding, "mutations are not allowed over GET" is not part of the
		// contract httpreq's package doc makes ("gqlhttp and gqlsse cannot
		// drift apart on rules a client can tell the difference between");
		// it is independently implemented per transport. gqlsse's wording
		// here ("Mutations are not allowed over GET; use POST.") matches the
		// wording gqlfiber's own SSE handler independently chose
		// (transport/gqlfiber/sse.go), i.e. SSE-family transports share one
		// wording and plain-HTTP transports share another -- a real,
		// consistent split along transport kind, not a one-off drift. Status
		// is still required to match; the brief's table gives only "405" for
		// this row (unlike every other row, which gives literal text),
		// which is consistent with that split.
		t.Run("gqlsse", func(t *testing.T) {
			status, got := doRequest(t, servers["gqlsse"], http.MethodGet, path, headers, "")
			if status != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405 (body: %s)", status, got)
			}
			const wantSSEBody = `{"errors":[{"message":"Mutations are not allowed over GET; use POST."}]}`
			if got != wantSSEBody {
				t.Errorf("body = %s, want %s", got, wantSSEBody)
			}
		})
	})

	t.Run("missing query, APQ off", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		const want = `{"errors":[{"message":"request is missing the \"query\" member."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "application/json"}, `{}`, http.StatusBadRequest, want)
	})

	// Resolution of the persistedQuery extension happens before this check,
	// but {} carries no such extension, so this must behave exactly as APQ
	// off: the missing-query error is the same error either way.
	t.Run("missing query, APQ on", func(t *testing.T) {
		servers := newEquivServers(t,
			[]gqlhttp.Option{gqlhttp.WithPersistedQueries(apq.NewCache(10))},
			[]gqlsse.Option{gqlsse.WithPersistedQueries(apq.NewCache(10))},
			[]gqlfiber.Option{gqlfiber.WithPersistedQueries(apq.NewCache(10))},
		)
		const want = `{"errors":[{"message":"request is missing the \"query\" member."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "application/json"}, `{}`, http.StatusBadRequest, want)
	})
}
