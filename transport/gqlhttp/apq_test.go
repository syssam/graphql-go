package gqlhttp_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/ext/apq"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

const apqSDL = `
type Query { ping: String! }
type Mutation { touch: String! }
`

func apqExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(apqSDL),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })),
		graphql.Mutation(graphql.Field("touch", func(graphql.Root) string { return "touched" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

func apqServer(t *testing.T, cache apq.Cache) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(gqlhttp.New(apqExecutor(t),
		gqlhttp.WithCSRFPrevention(false),
		gqlhttp.WithPersistedQueries(cache)))
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		srv.Close()
	})
	return srv, client
}

func extJSON(hash string) string {
	return fmt.Sprintf(`{"persistedQuery":{"version":1,"sha256Hash":%q}}`, hash)
}

func postJSON(t *testing.T, client *http.Client, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Persisted-query clients send this, and it is what selects the 200 they
	// expect for PersistedQueryNotFound.
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestAPQRoundTrip is the protocol as a client drives it: miss, register,
// then hit with the hash alone.
func TestAPQRoundTrip(t *testing.T) {
	srv, client := apqServer(t, apq.NewCache(10))
	const query = `{ ping }`
	hash := apq.Hash(query)

	status, body := postJSON(t, client, srv.URL, `{"extensions":`+extJSON(hash)+`}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 so the client reads the body and retries", status)
	}
	if !strings.Contains(body, "PersistedQueryNotFound") {
		t.Fatalf("body = %s", body)
	}

	status, body = postJSON(t, client, srv.URL,
		fmt.Sprintf(`{"query":%q,"extensions":%s}`, query, extJSON(hash)))
	if status != http.StatusOK || body != `{"data":{"ping":"pong"}}` {
		t.Fatalf("registration: status %d body %s", status, body)
	}

	status, body = postJSON(t, client, srv.URL, `{"extensions":`+extJSON(hash)+`}`)
	if status != http.StatusOK || body != `{"data":{"ping":"pong"}}` {
		t.Fatalf("hash-only request: status %d body %s", status, body)
	}
}

func TestAPQOverGET(t *testing.T) {
	srv, client := apqServer(t, apq.NewCache(10))
	const query = `{ ping }`
	hash := apq.Hash(query)

	get := func(extra string) (int, string) {
		t.Helper()
		resp, err := client.Get(srv.URL + "?extensions=" + url.QueryEscape(extJSON(hash)) + extra)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	// Registering over GET, then the short request a CDN can cache.
	if _, body := get("&query=" + url.QueryEscape(query)); body != `{"data":{"ping":"pong"}}` {
		t.Fatalf("registration over GET: %s", body)
	}
	if _, body := get(""); body != `{"data":{"ping":"pong"}}` {
		t.Fatalf("hash-only GET: %s", body)
	}
}

// TestPersistedMutationIsStillRejectedOverGET is why resolution happens during
// parsing rather than at execution: a request carrying only a hash has no
// query text, so a guard that runs first has nothing to inspect and would let
// a persisted mutation through a URL a browser can be led to open.
func TestPersistedMutationIsStillRejectedOverGET(t *testing.T) {
	cache := apq.NewCache(10)
	srv, client := apqServer(t, cache)
	const mutation = `mutation { touch }`
	hash := apq.Hash(mutation)

	// Register it the legitimate way, over POST.
	status, _ := postJSON(t, client, srv.URL,
		fmt.Sprintf(`{"query":%q,"extensions":%s}`, mutation, extJSON(hash)))
	if status != http.StatusOK {
		t.Fatalf("registration status = %d", status)
	}

	resp, err := client.Get(srv.URL + "?extensions=" + url.QueryEscape(extJSON(hash)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a persisted mutation ran over GET: status %d, body %s", resp.StatusCode, body)
	}
}

// TestAPQNotFoundIsBadRequestForSpecMediaType checks the other half of the
// status rule: a client asking for application/graphql-response+json gets the
// status the GraphQL over HTTP specification calls for.
func TestAPQNotFoundIsBadRequestForSpecMediaType(t *testing.T) {
	srv, client := apqServer(t, apq.NewCache(10))

	req, _ := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader(`{"extensions":`+extJSON(apq.Hash(`{ ping }`))+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/graphql-response+json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPQInBatch(t *testing.T) {
	const query = `{ ping }`
	hash := apq.Hash(query)
	cache := apq.NewCache(10)
	cache.Set(hash, query)

	srv := httptest.NewServer(gqlhttp.New(apqExecutor(t),
		gqlhttp.WithCSRFPrevention(false),
		gqlhttp.WithBatching(5),
		gqlhttp.WithPersistedQueries(cache)))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	_, body := postJSON(t, client, srv.URL,
		`[{"extensions":`+extJSON(hash)+`},{"query":"{ ping }"}]`)
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("body is not a batch: %s", body)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries: %s", len(entries), body)
	}
	for i, e := range entries {
		if string(e) != `{"data":{"ping":"pong"}}` {
			t.Fatalf("entry %d = %s", i, e)
		}
	}
}

// TestAPQDisabledByDefault keeps the extension inert unless asked for, so a
// handler built without the option cannot be made to execute stored text.
func TestAPQDisabledByDefault(t *testing.T) {
	srv := httptest.NewServer(gqlhttp.New(apqExecutor(t), gqlhttp.WithCSRFPrevention(false)))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	status, body := postJSON(t, client, srv.URL, `{"extensions":`+extJSON(apq.Hash(`{ ping }`))+`}`)
	if strings.Contains(body, "PersistedQueryNotFound") {
		t.Fatalf("APQ answered on a handler that did not enable it: %s", body)
	}
	if status != http.StatusOK && status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, "errors") {
		t.Fatalf("a request with no query should fail: %s", body)
	}
}
