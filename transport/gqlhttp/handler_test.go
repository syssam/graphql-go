package gqlhttp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

const sdl = `
type Query { hello(name: String): String! fail: String! }
type Mutation { inc: Int! }
`

type helloArgs struct{ Name *string }

func newHandler(t *testing.T, opts ...gqlhttp.Option) (*gqlhttp.Handler, *atomic.Int64) {
	t.Helper()
	var counter atomic.Int64
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Args[helloArgs](graphql.InputField("name", func(a *helloArgs, v *string) { a.Name = v })),
		graphql.Object[graphql.Root]("Query",
			graphql.FieldArgs("hello", func(_ graphql.Root, a helloArgs) string {
				if a.Name == nil {
					return "hello, world"
				}
				return "hello, " + *a.Name
			}),
			graphql.Resolve("fail", func(context.Context, graphql.Root) (string, error) { return "", errors.New("boom") }),
		),
		graphql.Object[graphql.Root]("Mutation",
			graphql.Resolve("inc", func(context.Context, graphql.Root) (int, error) { return int(counter.Add(1)), nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return gqlhttp.New(graphql.NewExecutor(s), opts...), &counter
}

type result struct {
	status      int
	contentType string
	body        string
	header      http.Header
}

func do(h http.Handler, req *http.Request) result {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return result{rec.Code, rec.Header().Get("Content-Type"), strings.TrimSpace(string(body)), rec.Header()}
}

func post(body string, headers ...string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	setHeaders(req, headers)
	return req
}

func get(params map[string]string, headers ...string) *http.Request {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	req := httptest.NewRequest(http.MethodGet, "/graphql?"+q.Encode(), nil)
	setHeaders(req, headers)
	return req
}

func setHeaders(req *http.Request, kv []string) {
	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}
}

func expect(t *testing.T, got result, status int, contentType, body string) {
	t.Helper()
	if got.status != status {
		t.Errorf("status = %d, want %d (body %s)", got.status, status, got.body)
	}
	if contentType != "" && got.contentType != contentType {
		t.Errorf("content-type = %q, want %q", got.contentType, contentType)
	}
	if body != "" && got.body != body {
		t.Errorf("body = %s, want %s", got.body, body)
	}
}

const gqlJSON = "application/graphql-response+json; charset=utf-8"
const plainJSON = "application/json; charset=utf-8"

func TestPostQuery(t *testing.T) {
	h, _ := newHandler(t)
	expect(t, do(h, post(`{"query":"{ hello }"}`)), 200, gqlJSON, `{"data":{"hello":"hello, world"}}`)
	expect(t, do(h, post(`{"query":"query A($n: String) { hello(name: $n) }","variables":{"n":"go"},"operationName":"A"}`)), 200, gqlJSON, `{"data":{"hello":"hello, go"}}`)
}

func TestPostFieldErrorsAre200(t *testing.T) {
	h, _ := newHandler(t)
	got := do(h, post(`{"query":"{ fail }"}`))
	expect(t, got, 200, gqlJSON, "")
	if !strings.Contains(got.body, `"errors":[{"message":"boom"`) || !strings.Contains(got.body, `"data":null`) {
		t.Fatalf("body = %s", got.body)
	}
}

func TestRequestErrorStatusDependsOnAccept(t *testing.T) {
	h, _ := newHandler(t)
	cases := []struct {
		accept string
		status int
		ct     string
	}{
		{"", 400, gqlJSON},
		{"*/*", 400, gqlJSON},
		{"application/graphql-response+json", 400, gqlJSON},
		{"application/json", 200, plainJSON},
		{"application/json, application/graphql-response+json", 400, gqlJSON},
		{"application/graphql-response+json;q=0.5, application/json", 200, plainJSON},
		{"application/json;q=0.5, application/graphql-response+json;q=0.9", 400, gqlJSON},
		{"text/html, application/*", 400, gqlJSON},
		{"application/*;q=0.8, application/json;q=0.9", 200, plainJSON},
	}
	for _, c := range cases {
		req := post(`{"query":"{ nope }"}`)
		if c.accept != "" {
			req.Header.Set("Accept", c.accept)
		}
		got := do(h, req)
		expect(t, got, c.status, c.ct, "")
		if !strings.Contains(got.body, `"code":"GRAPHQL_VALIDATION_FAILED"`) {
			t.Errorf("accept %q: body = %s", c.accept, got.body)
		}
	}
	got := do(h, post(`{"query":"{ hello }"}`, "Accept", "text/html"))
	expect(t, got, 406, "", "")
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := newHandler(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(m, "/graphql", nil)
		got := do(h, req)
		expect(t, got, 405, gqlJSON, "")
		if got.header.Get("Allow") != "GET, POST" {
			t.Errorf("%s: Allow = %q", m, got.header.Get("Allow"))
		}
	}
}

func TestGetQuery(t *testing.T) {
	h, _ := newHandler(t)
	got := do(h, get(map[string]string{
		"query":         `query A($n: String) { hello(name: $n) } query B { hello }`,
		"variables":     `{"n":"get & go"}`,
		"operationName": "A",
	}, "GraphQL-Require-Preflight", "1"))
	expect(t, got, 200, gqlJSON, `{"data":{"hello":"hello, get & go"}}`)

	got = do(h, get(map[string]string{"query": `{ hello }`}, "X-Requested-With", "XMLHttpRequest"))
	expect(t, got, 200, gqlJSON, `{"data":{"hello":"hello, world"}}`)

	got = do(h, get(map[string]string{"query": `{ hello }`}, "Content-Type", "application/json"))
	expect(t, got, 200, gqlJSON, `{"data":{"hello":"hello, world"}}`)
}

func TestCSRFPrevention(t *testing.T) {
	h, _ := newHandler(t)
	got := do(h, get(map[string]string{"query": `{ hello }`}))
	expect(t, got, 403, gqlJSON, "")
	if !strings.Contains(got.body, "GraphQL-Require-Preflight") {
		t.Fatalf("body = %s", got.body)
	}
	// POST with a simple content type is equally forgeable.
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ hello }"}`))
	req.Header.Set("Content-Type", "text/plain")
	expect(t, do(h, req), 403, gqlJSON, "")

	open, _ := newHandler(t, gqlhttp.WithCSRFPrevention(false))
	expect(t, do(open, get(map[string]string{"query": `{ hello }`})), 200, gqlJSON, `{"data":{"hello":"hello, world"}}`)

	custom, _ := newHandler(t, gqlhttp.WithCSRFPrevention(true, "X-Custom"))
	expect(t, do(custom, get(map[string]string{"query": `{ hello }`}, "GraphQL-Require-Preflight", "1")), 403, gqlJSON, "")
	expect(t, do(custom, get(map[string]string{"query": `{ hello }`}, "X-Custom", "y")), 200, gqlJSON, "")
}

func TestMutationOverGET(t *testing.T) {
	h, counter := newHandler(t)
	got := do(h, get(map[string]string{"query": `mutation { inc }`}, "GraphQL-Require-Preflight", "1"))
	expect(t, got, 405, gqlJSON, "")
	if !strings.Contains(got.body, "mutations are not allowed over GET") || counter.Load() != 0 {
		t.Fatalf("body = %s, counter = %d", got.body, counter.Load())
	}
	got = do(h, get(map[string]string{"query": `query Q { hello } mutation M { inc }`, "operationName": "M"}, "GraphQL-Require-Preflight", "1"))
	expect(t, got, 405, gqlJSON, "")
	expect(t, do(h, post(`{"query":"mutation { inc }"}`)), 200, gqlJSON, `{"data":{"inc":1}}`)
}

func TestUnsupportedMediaType(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`query=%7Bhello%7D`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("GraphQL-Require-Preflight", "1")
	expect(t, do(h, req), 415, gqlJSON, "")

	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ hello }"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	expect(t, do(h, req), 200, gqlJSON, `{"data":{"hello":"hello, world"}}`)
}

func TestBodyTooLarge(t *testing.T) {
	h, _ := newHandler(t, gqlhttp.WithMaxBodyBytes(64))
	expect(t, do(h, post(`{"query":"{ hello }"}`)), 200, gqlJSON, "")
	expect(t, do(h, post(`{"query":"{ hello }","variables":{"pad":"`+strings.Repeat("x", 100)+`"}}`)), 413, gqlJSON, "")
}

func TestMalformedRequests(t *testing.T) {
	h, _ := newHandler(t)
	for _, body := range []string{`{"query":`, `[]`, `{"variables":{}}`, `{"query":""}`, `{"query":42}`, `"str"`} {
		got := do(h, post(body))
		expect(t, got, 400, gqlJSON, "")
		if !strings.HasPrefix(got.body, `{"errors":[{"message":`) {
			t.Errorf("body %s: got %s", body, got.body)
		}
	}
	got := do(h, get(map[string]string{"query": `{ hello }`, "variables": `not json`}, "GraphQL-Require-Preflight", "1"))
	expect(t, got, 400, gqlJSON, "")
}

func TestBatching(t *testing.T) {
	h, counter := newHandler(t)
	got := do(h, post(`[{"query":"{ hello }"}]`))
	expect(t, got, 400, gqlJSON, "")
	if !strings.Contains(got.body, "batching is not enabled") {
		t.Fatalf("body = %s", got.body)
	}

	b, counter := newHandler(t, gqlhttp.WithBatching(2))
	got = do(b, post(`[{"query":"mutation { inc }"},{"query":"{ hello }"},{"query":"{ hello }"}]`))
	expect(t, got, 400, gqlJSON, "")
	if !strings.Contains(got.body, "batch of 3 exceeds the limit of 2") || counter.Load() != 0 {
		t.Fatalf("body = %s", got.body)
	}

	got = do(b, post(`[{"query":"mutation { inc }"},{"query":"{ nope }"}]`))
	expect(t, got, 200, gqlJSON, "")
	if !strings.HasPrefix(got.body, `[{"data":{"inc":1}},{"errors":[{"message":`) || !strings.HasSuffix(got.body, `]`) {
		t.Fatalf("body = %s", got.body)
	}
	expect(t, do(b, post(`[]`)), 200, gqlJSON, `[]`)
	expect(t, do(b, post(`{"query":"{ hello }"}`)), 200, gqlJSON, `{"data":{"hello":"hello, world"}}`)
}

func TestContextCancellationPropagates(t *testing.T) {
	h, _ := newHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := post(`{"query":"{ hello }"}`).WithContext(ctx)
	got := do(h, req)
	// The request context is passed through; a pre-cancelled context still
	// produces a well-formed GraphQL response.
	if got.status != 200 || !strings.HasPrefix(got.body, `{"errors":[`) {
		t.Fatalf("got %d %s", got.status, got.body)
	}
}
