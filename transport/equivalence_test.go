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
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/ext/trusted"
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

// badExtensionExecutor answers every request with an extension encoding/json
// cannot serialize, so the envelope fails to compose after the status header
// has gone out.
func badExtensionExecutor(t *testing.T) func(*testing.T) *graphql.Executor {
	t.Helper()
	return func(t *testing.T) *graphql.Executor {
		t.Helper()
		s, err := graphql.NewSchema(graphql.SDL(`
type Query { hello: String! }
type Mutation { bump: String! }
`),
			graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
			graphql.Mutation(graphql.Field("bump", func(graphql.Root) string { return "bumped" })),
		)
		if err != nil {
			t.Fatalf("NewSchema: %v", err)
		}
		return graphql.NewExecutor(s, graphql.WithOperationInterceptor(
			graphql.OperationInterceptorFunc(func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
				oc.SetExtension("bad", func() {})
				return next(ctx, oc)
			})))
	}
}

// badRequestErrorExecutor answers a request error carrying an extension
// encoding/json cannot serialize. A request interceptor is what reaches it:
// the operation chain never runs for a document that fails validation.
func badRequestErrorExecutor(t *testing.T) func(*testing.T) *graphql.Executor {
	t.Helper()
	return func(t *testing.T) *graphql.Executor {
		t.Helper()
		s, err := graphql.NewSchema(graphql.SDL(`type Query { hello: String! }`),
			graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })),
		)
		if err != nil {
			t.Fatalf("NewSchema: %v", err)
		}
		return graphql.NewExecutor(s, graphql.WithRequestInterceptor(
			graphql.RequestInterceptorFunc(func(ctx context.Context, req *graphql.Request, next graphql.RequestHandler) *graphql.Response {
				resp := next(ctx, req)
				if resp.Extensions == nil {
					resp.Extensions = map[string]any{}
				}
				resp.Extensions["bad"] = func() {}
				return resp
			})))
	}
}

// newEquivServersWithExecutor is newEquivServers with the executor swapped, so
// a case can drive every handler against a schema of its own.
func newEquivServersWithExecutor(t *testing.T, mk func(*testing.T) *graphql.Executor) map[string]string {
	t.Helper()

	httpSrv := httptest.NewServer(gqlhttp.New(mk(t)))
	t.Cleanup(httpSrv.Close)

	sseSrv := httptest.NewServer(gqlsse.New(mk(t)))
	t.Cleanup(sseSrv.Close)

	e := echo.New()
	e.Any("/graphql", gqlecho.New(mk(t)))
	echoSrv := httptest.NewServer(e)
	t.Cleanup(echoSrv.Close)

	eSSE := echo.New()
	eSSE.Any("/graphql", gqlecho.SSE(mk(t)))
	echoSSESrv := httptest.NewServer(eSSE)
	t.Cleanup(echoSSESrv.Close)

	app := fiber.New()
	app.All("/graphql", gqlfiber.New(mk(t)))
	fiberURL := startFiberEquiv(t, app)

	appSSE := fiber.New()
	appSSE.All("/graphql", gqlfiber.SSE(mk(t)))
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
			`Send a non-simple Content-Type or one of the headers GraphQL-Require-Preflight, Apollo-Require-Preflight, X-Requested-With."}]}`
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

	// A body carrying no query never became an operation, so it is 400 under
	// every Accept -- it is a malformed request, not a GraphQL request error,
	// and only the latter follows the media type. graphql-http answers 400
	// here under both media types while answering a validation error 200 under
	// application/json, which is the same split. Asserted under three Accept
	// headers because the negotiated type used to decide this: with persisted
	// queries on, an application/json client got 200.
	assertMissingQuery := func(t *testing.T, servers map[string]string) {
		t.Helper()
		const want = `{"errors":[{"message":"request is missing the \"query\" member."}]}`
		json := map[string]string{"Content-Type": "application/json"}
		assertAllEqual(t, servers, http.MethodPost, "/graphql", json, `{}`, http.StatusBadRequest, want)

		// Naming a JSON type only makes sense to the plain-HTTP family; the
		// SSE handlers answer 406 to anything but text/event-stream.
		for _, accept := range []string{"application/json", "application/graphql-response+json"} {
			assertEqualAcross(t, servers, httpFamily, http.MethodPost, "/graphql",
				map[string]string{"Content-Type": "application/json", "Accept": accept},
				`{}`, http.StatusBadRequest, want)
		}
	}

	t.Run("missing query, APQ off", func(t *testing.T) {
		assertMissingQuery(t, newEquivServers(t, nil, nil, nil))
	})

	// Resolution of the persistedQuery extension happens before this check,
	// but {} carries no such extension, so this must behave exactly as APQ
	// off: the missing-query error is the same error either way.
	t.Run("missing query, APQ on", func(t *testing.T) {
		assertMissingQuery(t, newEquivServers(t,
			[]gqlhttp.Option{gqlhttp.WithPersistedQueries(apq.NewCache(10))},
			[]gqlsse.Option{gqlsse.WithPersistedQueries(apq.NewCache(10))},
			[]gqlfiber.Option{gqlfiber.WithPersistedQueries(apq.NewCache(10))},
		))
	})

	// A response whose extensions cannot be serialized fails after the status
	// header is on the wire, so the transport cannot change the status -- but it
	// can still send a body, and an empty 200 is indistinguishable from success
	// to any client. graphql-js, run for comparison, has the same shape: what
	// JSON.stringify can drop it drops, what it cannot it throws on, and the
	// whole envelope is lost. Losing the envelope is the reference behaviour;
	// answering with nothing at all is not.
	//
	// The fallback is only safe because Response.WriteTo composes the envelope
	// before writing any of it, so a serialization failure leaves the body
	// empty. httpreq.WriteBody counts what was written and appends only when
	// nothing was, which keeps a failure *after* bytes went out -- a client
	// disconnecting -- from corrupting a partly-written response.
	t.Run("an unserializable extension still produces a parseable body", func(t *testing.T) {
		servers := newEquivServersWithExecutor(t, badExtensionExecutor(t))
		assertEqualAcross(t, servers, httpFamily, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "application/json"},
			`{"query":"{ hello }"}`, http.StatusOK,
			`{"errors":[{"message":"internal system error"}]}`)
	})

	// The same failure on a request error, which the SSE family answers
	// before any stream opens, as a plain body through its writeResponse. That
	// path wrote resp.WriteTo straight out and sent an empty 400, while the
	// plain-HTTP family already fell back.
	t.Run("an unserializable request error still produces a parseable body", func(t *testing.T) {
		servers := newEquivServersWithExecutor(t, badRequestErrorExecutor(t))
		headers := map[string]string{"Content-Type": "application/json"}
		const body = `{"query":"{ nope }"}`
		const want = `{"errors":[{"message":"internal system error"}]}`
		assertEqualAcross(t, servers, httpFamily, http.MethodPost, "/graphql", headers, body, http.StatusOK, want)
		assertEqualAcross(t, servers, sseFamily, http.MethodPost, "/graphql", headers, body, http.StatusBadRequest, want)
	})

	// On an open stream the fallback goes out as the event's payload, and the
	// stream still completes: the SSE family used to write "data: " with
	// nothing after it and end the response without a complete event.
	t.Run("an unserializable event still produces a parseable event", func(t *testing.T) {
		servers := newEquivServersWithExecutor(t, badExtensionExecutor(t))
		assertEqualAcross(t, servers, sseFamily, http.MethodPost, "/graphql",
			map[string]string{"Content-Type": "application/json"},
			`{"query":"{ hello }"}`, http.StatusOK,
			"event: next\ndata: {\"errors\":[{\"message\":\"internal system error\"}]}\n\nevent: complete\ndata:")
	})

	// JSON allows whitespace before a value, and the plain-HTTP handlers
	// skipped it (they look for a batch array first) while the SSE handlers
	// handed the untrimmed body to httpreq.Decode, which refused anything not
	// starting with '{'. The same body was 200 on one endpoint and 400 on the
	// other.
	t.Run("leading whitespace before the body", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		headers := map[string]string{"Content-Type": "application/json"}
		const lead = " \r\n\t"

		const wantMissing = `{"errors":[{"message":"request is missing the \"query\" member."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql", headers, lead+`{}`, http.StatusBadRequest, wantMissing)

		const body = lead + `{"query":"{ hello }"}`
		assertEqualAcross(t, servers, httpFamily, http.MethodPost, "/graphql", headers, body,
			http.StatusOK, `{"data":{"hello":"world"}}`)
		assertEqualAcross(t, servers, sseFamily, http.MethodPost, "/graphql", headers, body,
			http.StatusOK, "event: next\ndata: {\"data\":{\"hello\":\"world\"}}\n\nevent: complete\ndata:")

		const wantEmpty = `{"errors":[{"message":"request body is empty."}]}`
		assertAllEqual(t, servers, http.MethodPost, "/graphql", headers, lead, http.StatusBadRequest, wantEmpty)
	})

	// Every header in DefaultCSRFHeaders has to actually satisfy the check, in
	// every handler. The list is the interop surface: a client that sends the
	// header its own server documents and gets a 403 anyway has no way to tell
	// that from the server being down. Apollo-Require-Preflight was missing,
	// which refused Apollo Client from a GET -- the request shape automatic
	// persisted queries are built on.
	t.Run("every default CSRF header is accepted", func(t *testing.T) {
		servers := newEquivServers(t, nil, nil, nil)
		const want = `{"data":{"hello":"world"}}`
		for _, h := range gqlhttp.DefaultCSRFHeaders {
			t.Run(h, func(t *testing.T) {
				assertEqualAcross(t, servers, httpFamily, http.MethodGet,
					"/graphql?query="+url.QueryEscape("{ hello }"),
					map[string]string{"Accept": "application/json", h: "1"},
					"", http.StatusOK, want)
			})
		}
	})

	// The first request an Apollo client ever sends: a hash over GET against a
	// cold cache. It must come back PersistedQueryNotFound with **200** for
	// application/json, because that is the answer the client retries after --
	// a 4xx or 5xx here and APQ never gets off the ground. Every existing APQ
	// test registers the query first, so the miss over GET was the one path
	// nothing drove, in any handler.
	t.Run("APQ miss over GET is 200 for application/json", func(t *testing.T) {
		servers := newEquivServers(t,
			[]gqlhttp.Option{gqlhttp.WithPersistedQueries(apq.NewCache(10))},
			[]gqlsse.Option{gqlsse.WithPersistedQueries(apq.NewCache(10))},
			[]gqlfiber.Option{gqlfiber.WithPersistedQueries(apq.NewCache(10))},
		)
		ext := url.QueryEscape(`{"persistedQuery":{"version":1,"sha256Hash":"` +
			apq.Hash(`{ hello }`) + `"}}`)
		const want = `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`
		assertEqualAcross(t, servers, httpFamily, http.MethodGet, "/graphql?extensions="+ext,
			map[string]string{"Accept": "application/json", "GraphQL-Require-Preflight": "1"}, "", http.StatusOK, want)
	})

	// The same answer under the specification media type. Read literally the
	// specification would make this 4xx, since the request did not execute --
	// but automatic persisted queries are not in the specification at all,
	// they are Apollo's protocol, and Apollo Server answers the miss with 200
	// whatever media type was negotiated. The status has to follow the
	// protocol, not the media type: every client that sends a persistedQuery
	// extension is an Apollo-protocol client, and a 4xx it reports as a failed
	// request is a cold cache that never warms.
	//
	// This asserted 400 until a differential against Apollo Server 5.5.1 found
	// it. Apollo Client sends `Accept: application/graphql-response+json` (and
	// `*/*` reaches the same branch), so the 400 was what real clients got.
	t.Run("APQ miss over GET is 200 for the spec media type too", func(t *testing.T) {
		servers := newEquivServers(t,
			[]gqlhttp.Option{gqlhttp.WithPersistedQueries(apq.NewCache(10))},
			[]gqlsse.Option{gqlsse.WithPersistedQueries(apq.NewCache(10))},
			[]gqlfiber.Option{gqlfiber.WithPersistedQueries(apq.NewCache(10))},
		)
		ext := url.QueryEscape(`{"persistedQuery":{"version":1,"sha256Hash":"` +
			apq.Hash(`{ hello }`) + `"}}`)
		const want = `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`
		for _, accept := range []string{"application/graphql-response+json", "*/*"} {
			t.Run(accept, func(t *testing.T) {
				assertEqualAcross(t, servers, httpFamily, http.MethodGet, "/graphql?extensions="+ext,
					map[string]string{"Accept": accept, "GraphQL-Require-Preflight": "1"}, "",
					http.StatusOK, want)
			})
		}
	})

	// A safelist refusal is not a handshake: the client must not retry with
	// the query text, which is the thing the safelist forbids, so it keeps the
	// status its media type calls for.
	t.Run("safelist refusal stays 400 for the spec media type", func(t *testing.T) {
		store := trusted.NewStore(map[string]string{})
		servers := newEquivServers(t,
			[]gqlhttp.Option{gqlhttp.WithPersistedQueries(store)},
			[]gqlsse.Option{gqlsse.WithPersistedQueries(store)},
			[]gqlfiber.Option{gqlfiber.WithPersistedQueries(store)},
		)
		// The query text is what a safelist refuses; an unknown hash alone is
		// still answered PersistedQueryNotFound, so it would not reach the
		// branch under test.
		ext := url.QueryEscape(`{"persistedQuery":{"version":1,"sha256Hash":"` +
			apq.Hash(`{ hello }`) + `"}}`)
		const want = `{"errors":[{"message":"PersistedQueryNotInList","extensions":{"code":"PERSISTED_QUERY_NOT_IN_LIST"}}]}`
		assertEqualAcross(t, servers, httpFamily, http.MethodGet,
			"/graphql?query="+url.QueryEscape(`{ hello }`)+"&extensions="+ext,
			map[string]string{"Accept": "application/graphql-response+json", "GraphQL-Require-Preflight": "1"}, "",
			http.StatusBadRequest, want)
	})
}
