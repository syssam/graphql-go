package gqlecho

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// serve must not hide the underlying writer's Flusher from
// http.ResponseController: transport/gqlsse flushes after every event, and a
// statusRecorder that only promotes the http.ResponseWriter interface breaks
// that silently -- Flush becomes a no-op instead of a compile or runtime
// error, so nothing but this test would catch it.
func TestServeUnwrapsFlusher(t *testing.T) {
	var flushErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flushErr = http.NewResponseController(w).Flush()
	})

	rec := httptest.NewRecorder()
	c := echo.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := serve(c, h); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if flushErr != nil {
		t.Fatalf("Flush() through serve's writer: %v, want nil", flushErr)
	}
}
