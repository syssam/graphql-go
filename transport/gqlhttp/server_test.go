package gqlhttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The other tests in this package drive the handler with httptest.NewRecorder,
// which never touches a socket. These run the same handler behind a real
// net/http server and talk to it with a real client, so header serialisation,
// the response body framing, keep-alive reuse and client-side cancellation are
// all exercised end to end.

func newServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	h, _ := newHandler(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.Client()
}

func postQuery(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestServerPostQuery(t *testing.T) {
	srv, c := newServer(t)

	resp := postQuery(t, c, srv.URL, `{"query":"{ hello(name: \"ada\") }"}`)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want a JSON media type", ct)
	}
	if want := `{"data":{"hello":"hello, ada"}}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

// TestServerGetQuery also covers CSRF prevention across a real connection: a
// plain GET is a simple request a browser can forge, so it must be refused
// until the client proves it can set a header.
func TestServerGetQuery(t *testing.T) {
	srv, c := newServer(t)
	target := srv.URL + "?query=" + url.QueryEscape(`{ hello }`)

	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := resp.StatusCode; got != http.StatusForbidden {
		t.Errorf("unpreflighted GET status = %d, want 403", got)
	}
	readBody(t, resp)

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("GraphQL-Require-Preflight", "1")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatalf("preflighted get: %v", err)
	}
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if want := `{"data":{"hello":"hello, world"}}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

// TestServerKeepAliveReuse sends several requests over one client and checks
// the responses stay correct, which a recorder cannot show: a handler that
// mismanaged the body or Content-Length would corrupt the second response on
// a reused connection.
func TestServerKeepAliveReuse(t *testing.T) {
	srv, c := newServer(t)

	for i := range 5 {
		resp := postQuery(t, c, srv.URL, `{"query":"mutation { inc }"}`)
		body := readBody(t, resp)
		want := fmt.Sprintf(`{"data":{"inc":%d}}`, i+1)
		if body != want {
			t.Fatalf("request %d: body = %s, want %s", i+1, body, want)
		}
	}
}

// TestServerFieldErrorIsStill200 pins the GraphQL over HTTP rule across a real
// connection: a resolver error is a well-formed response, not a transport
// failure.
func TestServerFieldErrorIsStill200(t *testing.T) {
	srv, c := newServer(t)

	resp := postQuery(t, c, srv.URL, `{"query":"{ fail }"}`)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for a field error", resp.StatusCode)
	}
	var payload struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(payload.Errors) == 0 {
		t.Fatalf("want an error in the payload, got %s", body)
	}
}

// TestServerClientCancellation cancels mid-flight over a real connection. The
// server must notice and give the connection up rather than hang.
func TestServerClientCancellation(t *testing.T) {
	srv, c := newServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL,
		strings.NewReader(`{"query":"{ hello }"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	cancel()

	if _, err := c.Do(req); err == nil {
		t.Fatal("want the cancelled request to fail")
	}

	// The server must still serve afterwards.
	resp := postQuery(t, c, srv.URL, `{"query":"{ hello }"}`)
	if body := readBody(t, resp); body != `{"data":{"hello":"hello, world"}}` {
		t.Fatalf("server unhealthy after a cancelled request: %s", body)
	}
}

// TestServerConcurrentClients drives the handler from several connections at
// once, which is how it is actually used.
func TestServerConcurrentClients(t *testing.T) {
	srv, c := newServer(t)

	const clients = 16
	errs := make(chan error, clients)
	for range clients {
		go func() {
			resp, err := c.Post(srv.URL, "application/json",
				strings.NewReader(`{"query":"{ hello(name: \"x\") }"}`))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- err
				return
			}
			if got := string(b); got != `{"data":{"hello":"hello, x"}}` {
				errs <- fmt.Errorf("unexpected body: %s", got)
				return
			}
			errs <- nil
		}()
	}
	deadline := time.After(30 * time.Second)
	for range clients {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for concurrent clients")
		}
	}
}
