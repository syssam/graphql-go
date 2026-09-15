package gqlfiber

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// do routes one request through an app serving GraphQL on every method at
// /graphql, and returns the status and the trimmed body.
func do(t *testing.T, req *http.Request, opts ...Option) (int, string) {
	t.Helper()
	app := fiber.New()
	app.All("/graphql", New(newTestExecutor(t), opts...))

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, strings.TrimSpace(string(body))
}

func postJSON(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestPostQuery(t *testing.T) {
	status, body := do(t, postJSON("/graphql", `{"query":"{hello}"}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if want := `{"data":{"hello":"world"}}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestGetQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/graphql?query=%7Bhello%7D", nil)
	req.Header.Set("GraphQL-Require-Preflight", "1")

	status, body := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if want := `{"data":{"hello":"world"}}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestOverLongBodyIs413(t *testing.T) {
	status, body := do(t, postJSON("/graphql", `{"query":"`+strings.Repeat("x", 200)+`"}`), WithMaxBodyBytes(16))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", status, body)
	}
	if want := `{"errors":[{"message":"request body exceeds 16 bytes."}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

// A mutation reachable by URL is a mutation a browser can be led to run, so
// the handler refuses it before planning the operation.
func TestMutationOverGetIs405(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/graphql?query=mutation%7Bbump%7D", nil)
	req.Header.Set("GraphQL-Require-Preflight", "1")

	status, body := do(t, req)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", status, body)
	}
	if want := `{"errors":[{"message":"mutation operations (mutations are not allowed over GET); use POST."}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestForgeableRequestIs403(t *testing.T) {
	status, body := do(t, httptest.NewRequest(http.MethodGet, "/graphql?query=%7Bhello%7D", nil))
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", status, body)
	}
	const want = `{"errors":[{"message":"This request could be forged cross-site. ` +
		`Send a non-simple Content-Type or one of the headers GraphQL-Require-Preflight, X-Requested-With."}]}`
	if body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestNonJSONPostIs415(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{hello}"}`))
	req.Header.Set("Content-Type", "application/graphql")

	status, body := do(t, req)
	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415: %s", status, body)
	}
	if want := `{"errors":[{"message":"Content-Type must be application/json."}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestRejectedMethodIs405(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/graphql", nil)
	req.Header.Set("GraphQL-Require-Preflight", "1")

	status, body := do(t, req)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", status, body)
	}
	if want := `{"errors":[{"message":"Method DELETE is not allowed; use GET or POST."}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestUnacceptableAcceptIs406(t *testing.T) {
	req := postJSON("/graphql", `{"query":"{hello}"}`)
	req.Header.Set("Accept", "text/html")

	status, body := do(t, req)
	if status != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406: %s", status, body)
	}
	const want = `{"errors":[{"message":"Accept header does not allow ` +
		`application/graphql-response+json or application/json."}]}`
	if body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

// application/graphql-response+json makes a request error an HTTP error;
// application/json keeps the 200 that older clients expect.
func TestRequestErrorStatusFollowsTheNegotiatedType(t *testing.T) {
	for _, tc := range []struct {
		accept string
		status int
	}{
		{"application/graphql-response+json", http.StatusBadRequest},
		{"application/json", http.StatusOK},
	} {
		req := postJSON("/graphql", `{"query":"{nope}"}`)
		req.Header.Set("Accept", tc.accept)

		status, body := do(t, req)
		if status != tc.status {
			t.Errorf("Accept %s: status = %d, want %d: %s", tc.accept, status, tc.status, body)
		}
		if !strings.HasPrefix(body, `{"errors":[{"message":`) {
			t.Errorf("Accept %s: body = %s, want an error envelope", tc.accept, body)
		}
	}
}

func TestBatchingIsOffByDefault(t *testing.T) {
	status, body := do(t, postJSON("/graphql", `[{"query":"{hello}"}]`))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if want := `{"errors":[{"message":"batching is not enabled on this server."}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestBatchExecutesEachEntry(t *testing.T) {
	status, body := do(t, postJSON("/graphql", `[{"query":"{hello}"},{"query":"{hello}"}]`), WithBatching(2))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if want := `[{"data":{"hello":"world"}},{"data":{"hello":"world"}}]`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestResponseContentType(t *testing.T) {
	app := fiber.New()
	app.Post("/graphql", New(newTestExecutor(t)))

	resp, err := app.Test(postJSON("/graphql", `{"query":"{hello}"}`))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.Header.Get("Content-Type"), "application/graphql-response+json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}
