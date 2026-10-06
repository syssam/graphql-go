package httpreq

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// headerSource is a Source made of headers alone, each with as many values
// as the request carried.
type headerSource map[string][]string

func (h headerSource) Method() string             { return http.MethodPost }
func (h headerSource) QueryParam(string) string   { return "" }
func (h headerSource) Body(int64) ([]byte, error) { return nil, nil }
func (h headerSource) Repeated(name string) bool  { return len(h[name]) > 1 }
func (h headerSource) Header(name string) string {
	if v := h[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func contentType(values ...string) headerSource { return headerSource{"Content-Type": values} }

// Forgeable is the CSRF rule, and every row is a request a browser either can
// or cannot send cross-origin without a preflight. The three "simple" types
// are each a row because a POST of any of them is refused again by
// RequireJSONBody, so dropping one from the rule changes no handler's answer
// to a POST and nothing but this table notices.
func TestForgeable(t *testing.T) {
	with := func(src headerSource, name, value string) headerSource {
		out := headerSource{name: {value}}
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name string
		src  headerSource
		want bool
	}{
		{"no Content-Type", headerSource{}, true},
		{"text/plain", contentType("text/plain"), true},
		{"text/plain with a charset", contentType("text/plain; charset=utf-8"), true},
		{"text/plain in upper case", contentType("TEXT/PLAIN"), true},
		{"a form", contentType("application/x-www-form-urlencoded"), true},
		{"a multipart form", contentType("multipart/form-data; boundary=x"), true},
		{"JSON named only in a parameter", contentType("text/plain; charset=application/json"), true},
		{"a list of types", contentType("application/json, text/plain"), true},
		{"no type at all", contentType(";"), true},
		{"a parameter with no value", contentType("application/json; charset"), true},
		{"JSON", contentType("application/json"), false},
		{"JSON with a charset", contentType("application/json; charset=utf-8"), false},
		{"JSON in upper case", contentType("APPLICATION/JSON"), false},
		{"another type that needs a preflight", contentType("application/xml"), false},
		{"Content-Type sent twice, JSON first", contentType("application/json", "text/plain"), true},
		{"Content-Type sent twice, JSON last", contentType("text/plain", "application/json"), true},
		{"Content-Type sent twice, JSON both times", contentType("application/json", "application/json"), true},
		{"the specification's header", headerSource{"GraphQL-Require-Preflight": {"1"}}, false},
		{"Apollo's header", headerSource{"Apollo-Require-Preflight": {"true"}}, false},
		{"X-Requested-With", headerSource{"X-Requested-With": {"XMLHttpRequest"}}, false},
		{"a preflight header beside a simple type", with(contentType("text/plain"), "GraphQL-Require-Preflight", "1"), false},
		{"a preflight header beside a repeated type", with(contentType("text/plain", "application/json"), "GraphQL-Require-Preflight", "1"), false},
		{"a preflight header with no value", headerSource{"GraphQL-Require-Preflight": {""}}, true},
		{"a header that is not on the list", headerSource{"X-Custom": {"1"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Forgeable(tc.src, new(ContentType), DefaultCSRFHeaders); got != tc.want {
				t.Errorf("Forgeable = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a configured list replaces the default", func(t *testing.T) {
		if Forgeable(headerSource{"X-Custom": {"1"}}, new(ContentType), []string{"X-Custom"}) {
			t.Error("the configured header did not satisfy the check")
		}
		if !Forgeable(headerSource{"GraphQL-Require-Preflight": {"1"}}, new(ContentType), []string{"X-Custom"}) {
			t.Error("a default header satisfied a check configured without it")
		}
	})
}

func TestRequireJSONBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  headerSource
		ok   bool
	}{
		{"JSON", contentType("application/json"), true},
		{"JSON with a charset", contentType("application/json; charset=utf-8"), true},
		{"JSON in upper case", contentType("APPLICATION/JSON"), true},
		{"no Content-Type", headerSource{}, false},
		{"text/plain", contentType("text/plain"), false},
		{"a list of types", contentType("application/json, text/plain"), false},
		// net/http reads the first of two and fasthttp the last, so a repeated
		// header has no value the handlers agree on.
		{"sent twice, JSON first", contentType("application/json", "text/plain"), false},
		{"sent twice, JSON last", contentType("text/plain", "application/json"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RequireJSONBody(tc.src, new(ContentType)); (err == nil) != tc.ok {
				t.Errorf("RequireJSONBody = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// readCounter counts how often the Content-Type was asked for, either way.
type readCounter struct {
	headerSource
	reads *int
}

func (r readCounter) Repeated(name string) bool {
	*r.reads++
	return r.headerSource.Repeated(name)
}

func (r readCounter) Header(name string) string {
	if name == "Content-Type" {
		*r.reads++
	}
	return r.headerSource.Header(name)
}

// A POST is asked about its Content-Type twice, by the CSRF rule and by the
// body rule, and on Fiber each asking scanned every raw header line. One
// ContentType answers both from a single reading, and a request the CSRF rule
// passes on a preflight header alone is not read at all.
func TestContentTypeIsReadOncePerRequest(t *testing.T) {
	reads := 0
	src := readCounter{contentType("application/json"), &reads}
	var ct ContentType
	if Forgeable(src, &ct, DefaultCSRFHeaders) {
		t.Fatal("a JSON POST was taken for forgeable")
	}
	if err := RequireJSONBody(src, &ct); err != nil {
		t.Fatalf("RequireJSONBody = %v", err)
	}
	// Once whether it is repeated, once for its value.
	if reads != 2 {
		t.Errorf("the Content-Type was read %d times over both rules, want 2", reads)
	}

	reads = 0
	src = readCounter{headerSource{"Apollo-Require-Preflight": {"true"}}, &reads}
	if Forgeable(src, new(ContentType), DefaultCSRFHeaders) {
		t.Fatal("a request with a preflight header was taken for forgeable")
	}
	if reads != 0 {
		t.Errorf("the Content-Type was read %d times for a request a preflight header had already passed", reads)
	}

	// The answer kept is the one a second rule must get, a refusal included.
	ct = ContentType{}
	twice := contentType("application/json", "application/json")
	if !Forgeable(twice, &ct, DefaultCSRFHeaders) || RequireJSONBody(twice, &ct) == nil {
		t.Error("a Content-Type sent twice was accepted by one of the two rules")
	}
}

func TestFromRequestReportsARepeatedHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	r.Header.Add("Content-Type", "text/plain")
	src := FromRequest(httptest.NewRecorder(), r)
	if src.Repeated("Content-Type") {
		t.Error("a header sent once was reported as repeated")
	}
	r.Header.Add("content-type", "application/json")
	if !src.Repeated("Content-Type") {
		t.Error("a header sent twice was not reported as repeated")
	}
}

// A member of the wrong JSON type is the client's mistake and is told so in
// the request's own terms. encoding/json words it in the server's: the Go
// struct, field and type it was decoding into.
func TestDecodeErrorsDoNotNameGoTypes(t *testing.T) {
	leaks := func(t *testing.T, msg string) {
		t.Helper()
		for _, s := range []string{"Go ", "struct", "interface", "map[", "json:", "graphql."} {
			if strings.Contains(msg, s) {
				t.Errorf("message %q contains %q", msg, s)
			}
		}
	}
	for body, want := range map[string]string{
		`{"query":5}`:                         `invalid JSON body: "query" must be a string, not a number.`,
		`{"query":"{a}","operationName":[1]}`: `invalid JSON body: "operationName" must be a string, not an array.`,
		`{"query":"{a}","extensions":[1]}`:    `invalid JSON body: "extensions" must be an object, not an array.`,
		`{"query":"{a}","extensions":"x"}`:    `invalid JSON body: "extensions" must be an object, not a string.`,
		`{"query":{"a":1}}`:                   `invalid JSON body: "query" must be a string, not an object.`,
		`{"query":true}`:                      `invalid JSON body: "query" must be a string, not a boolean.`,
	} {
		_, err := Decode([]byte(body), false)
		if err == nil {
			t.Errorf("Decode(%s) succeeded", body)
			continue
		}
		leaks(t, err.Error())
		if err.Error() != want {
			t.Errorf("Decode(%s) = %q, want %q", body, err, want)
		}
	}

	// A syntax error names a byte of the body and nothing of the server.
	_, err := Decode([]byte(`{"query":"{a}"}x`), false)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid JSON body: ") {
		t.Errorf("trailing garbage: %v", err)
	} else {
		leaks(t, err.Error())
	}

	for _, ext := range []string{`[1]`, `"x"`, `5`} {
		r := httptest.NewRequest(http.MethodGet, "/graphql?query=%7Ba%7D", nil)
		q := r.URL.Query()
		q.Set("extensions", ext)
		r.URL.RawQuery = q.Encode()
		_, err := ParseGET(FromRequest(httptest.NewRecorder(), r), false)
		if err == nil {
			t.Errorf("ParseGET(extensions=%s) succeeded", ext)
			continue
		}
		leaks(t, err.Error())
	}
}
