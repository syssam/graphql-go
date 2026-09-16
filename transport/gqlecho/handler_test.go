package gqlecho_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/syssam/graphql-go/transport/gqlecho"
)

func TestPostQuery(t *testing.T) {
	e := echo.New()
	e.POST("/graphql", gqlecho.New(newTestExecutor(t)))

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{hello}"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"data":{"hello":"world"}}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// A 400 from gqlhttp carries a GraphQL response envelope, not a
// transport-level failure -- it must reach the client as that envelope, not
// be swallowed and replaced by Echo's error page. This guards the switch in
// serve against ever adding http.StatusBadRequest to the mapped statuses.
func TestMalformedQueryIsNotAnEchoHTTPError(t *testing.T) {
	var seen error
	e := echo.New()
	e.HTTPErrorHandler = func(c *echo.Context, err error) { seen = err }
	e.POST("/graphql", gqlecho.New(newTestExecutor(t)))

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if seen != nil {
		t.Fatalf("HTTPErrorHandler invoked with %#v, want it untouched", seen)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.HasPrefix(body, `{"errors":[{"message":`) {
		t.Errorf("body = %s, want a GraphQL error envelope", body)
	}
}

// A method the handler rejects must reach Echo as an HTTPError, so that an
// application's error handler and middleware observe it. This is the only
// behavioural reason to prefer this package over echo.WrapHandler.
func TestRejectedMethodIsAnEchoHTTPError(t *testing.T) {
	var seen error
	e := echo.New()
	e.HTTPErrorHandler = func(c *echo.Context, err error) { seen = err }
	e.Add(http.MethodDelete, "/graphql", gqlecho.New(newTestExecutor(t)))

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/graphql", nil))

	he, ok := seen.(*echo.HTTPError)
	if !ok {
		t.Fatalf("error = %#v, want *echo.HTTPError", seen)
	}
	if he.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", he.Code)
	}
}
