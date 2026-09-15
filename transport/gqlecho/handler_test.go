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
