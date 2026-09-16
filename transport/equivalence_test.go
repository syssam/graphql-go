// This file proves, from a client's point of view, the claim internal/httpreq
// makes for itself: the CSRF check, the body limit and JSON-body decoding
// cannot drift apart between transports. It drives real HTTP requests
// through all six HTTP-carrying handlers -- gqlhttp, gqlsse, gqlecho.New,
// gqlecho.SSE, gqlfiber.New and gqlfiber.SSE -- over real listeners, and
// asserts identical status and body.
//
// The SSE handlers are rows here because gqlfiber's is a third hand-written
// copy of the check order with its messages retyped, and establishing that it
// still answers what gqlsse answers by reading the two files is exactly the
// inspection this file exists to replace.
//
// Every transport is driven the same way, over a real loopback listener,
// rather than mixing httptest.NewRecorder (in-process) for the net/http ones
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
// different code path than the other transports' real listeners.
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

	eSSE := echo.New()
	eSSE.Any("/graphql", gqlecho.SSE(newEquivExecutor(t), sseOpts...))
	echoSSESrv := httptest.NewServer(eSSE)
	t.Cleanup(echoSSESrv.Close)

	app := fiber.New()
	app.All("/graphql", gqlfiber.New(newEquivExecutor(t), fiberOpts...))
	fiberURL := startFiberEquiv(t, app)

	appSSE := fiber.New()
	appSSE.All("/graphql", gqlfiber.SSE(newEquivExecutor(t), fiberOpts...))
	fiberSSEURL := startFiberEquiv(t, appSSE)

	return map[string]string{
		"gqlhttp":      httpSrv.URL,
		"gqlsse":       sseSrv.URL,
		"gqlecho":      echoSrv.URL,
		"gqlecho.SSE":  echoSSESrv.URL,
		"gqlfiber":     fiberURL,
		"gqlfiber.SSE": fiberSSEURL,
	}
}

// transportNames fixes iteration order so a failing subtest is reported
// under a stable, predictable name instead of Go's randomised map order.
var transportNames = []string{"gqlhttp", "gqlsse", "gqlecho", "gqlecho.SSE", "gqlfiber", "gqlfiber.SSE"}

// The two families a handful of rows split along. A rejection composed by the
// transport itself rather than by internal/httpreq -- "mutations are not
// allowed over GET", the unacceptable-Accept message -- is worded per
// protocol, and the split is between GraphQL-over-HTTP and graphql-sse, not
// between one implementation and another. A row that splits must still assert
// every handler on both sides of it, or a third wording could hide there.
var (
	httpFamily = []string{"gqlhttp", "gqlecho", "gqlfiber"}
	sseFamily  = []string{"gqlsse", "gqlecho.SSE", "gqlfiber.SSE"}
)

// equivClient bounds the wait for a response head. Every case in this file is
// a rejection, answered in full before Do returns; a handler that instead
// opened a stream, or answered nothing, would otherwise hang until the whole
// package's ten-minute timeout dumped every goroutine in the process, rather
// than failing in seconds and naming the transport.
var equivClient = &http.Client{
	Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second},
	Timeout:   10 * time.Second,
}

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
	resp, err := equivClient.Do(req)
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

// assertEqualAcross drives the same request through each named transport and
// asserts each one answers with wantStatus and wantBody literally -- not
// merely with each other, so a bug shared by all of them still fails the
// test. Each transport runs as its own subtest, so a mismatch names exactly
// which transport diverged.
func assertEqualAcross(t *testing.T, servers map[string]string, names []string, method, path string, headers map[string]string, body string, wantStatus int, wantBody string) {
	t.Helper()
	for _, name := range names {
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

// assertAllEqual asserts the same answer from every transport in the table.
func assertAllEqual(t *testing.T, servers map[string]string, method, path string, headers map[string]string, body string, wantStatus int, wantBody string) {
	t.Helper()
	assertEqualAcross(t, servers, transportNames, method, path, headers, body, wantStatus, wantBody)
}

// TestEquivalence drives one table of requests through all six HTTP-carrying
// handlers and asserts identical status and identical body, proving that
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

	// The Accept check runs before everything else in both families, so this
	// row needs no valid request behind it. The wording splits: a
	// GraphQL-over-HTTP handler names the two types it negotiates between, an
	// SSE handler names the one it streams.
	t.Run("unacceptable Accept", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		headers := map[string]string{"Content-Type": "application/json", "Accept": "application/xml"}
		const body = `{"query":"{hello}"}`

		const wantHTTP = `{"errors":[{"message":"Accept header does not allow ` +
			`application/graphql-response+json or application/json."}]}`
		assertEqualAcross(t, servers, httpFamily, http.MethodPost, "/graphql", headers, body, http.StatusNotAcceptable, wantHTTP)

		const wantSSE = `{"errors":[{"message":"Accept header does not allow text/event-stream."}]}`
		assertEqualAcross(t, servers, sseFamily, http.MethodPost, "/graphql", headers, body, http.StatusNotAcceptable, wantSSE)
	})

	// The negotiated media type is client-visible twice: it names the response
	// Content-Type, and it decides whether a request error is 200 or 400. This
	// Accept header exercises the tie-break inside httpreq.Negotiate -- an
	// explicit application/json outranks a wildcard at equal q -- through the
	// status, so a wildcard that won instead would answer 400 here.
	//
	// The SSE family negotiates nothing: its Accept handling is the
	// all-or-nothing check above, and a request error is always 400 on the
	// media type it uses before a stream opens. Asserting that here is what
	// keeps the difference deliberate.
	t.Run("Accept wildcard tie-break", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		headers := map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json;q=0.9, */*;q=0.9",
		}
		const body = `{"query":"{nope}"}`
		const want = `{"errors":[{"message":"Cannot query field \"nope\" on type \"Query\".",` +
			`"locations":[{"line":1,"column":2}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`

		assertEqualAcross(t, servers, httpFamily, http.MethodPost, "/graphql", headers, body, http.StatusOK, want)
		assertEqualAcross(t, servers, sseFamily, http.MethodPost, "/graphql", headers, body, http.StatusBadRequest, want)
	})

	// The SSE family's rejection message for this case is asserted separately
	// from the plain-HTTP family's: see the comment below for why that is a
	// legitimate difference rather than a bug being papered over.
	t.Run("mutation over GET", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		headers := map[string]string{"GraphQL-Require-Preflight": "1"}
		const path = "/graphql?query=mutation%7Bbump%7D"

		const wantHTTP = `{"errors":[{"message":"mutation operations (mutations are not allowed over GET); use POST."}]}`
		assertEqualAcross(t, servers, httpFamily, http.MethodGet, path, headers, "", http.StatusMethodNotAllowed, wantHTTP)

		// The SSE handlers compose this rejection themselves rather than
		// through internal/httpreq -- unlike the CSRF check, the body limit
		// and JSON decoding, "mutations are not allowed over GET" is not part
		// of the contract httpreq's package doc makes ("the HTTP transports
		// cannot drift apart on rules a client can tell the difference
		// between"); it is independently implemented per transport. The
		// wording gqlsse chose ("Mutations are not allowed over GET; use
		// POST.") is the wording gqlfiber's own SSE handler independently
		// chose, and gqlecho.SSE inherits gqlsse's -- i.e. SSE-family
		// transports share one wording and plain-HTTP transports share
		// another, a real, consistent split along transport kind rather than
		// a one-off drift. Status is still required to match; the brief's
		// table gives only "405" for this row (unlike every other row, which
		// gives literal text), which is consistent with that split.
		const wantSSE = `{"errors":[{"message":"Mutations are not allowed over GET; use POST."}]}`
		assertEqualAcross(t, servers, sseFamily, http.MethodGet, path, headers, "", http.StatusMethodNotAllowed, wantSSE)
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
