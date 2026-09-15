package httpreq

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromRequestReadsTheRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/graphql?query=%7Ba%7D", strings.NewReader(`{"query":"{a}"}`))
	r.Header.Set("Content-Type", "application/json")
	src := FromRequest(httptest.NewRecorder(), r)

	if got := src.Method(); got != http.MethodPost {
		t.Errorf("Method() = %q, want POST", got)
	}
	if got := src.Header("Content-Type"); got != "application/json" {
		t.Errorf("Header() = %q, want application/json", got)
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
}

// An over-long body must be reportable as such, because ReadBody turns this
// into a 413 that every transport has to produce identically.
func TestFromRequestBodyTooLarge(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(strings.Repeat("x", 100)))
	src := FromRequest(httptest.NewRecorder(), r)

	if _, err := src.Body(10); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Body() error = %v, want ErrBodyTooLarge", err)
	}
}
