package transport_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

type answer struct {
	status      int
	contentType string
	body        string
}

// ask sends one request and returns everything a client can see of the
// answer. The URL is taken as written, so a probe can send a query string no
// URL builder would produce.
func ask(t *testing.T, base, method, target string, headers map[string]string, body string) answer {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+target, reader)
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
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return answer{resp.StatusCode, strings.TrimSpace(ct), strings.TrimSpace(string(b))}
}

// agree requires every handler of a family to give the first one's answer.
func agree(t *testing.T, servers map[string]string, family []string, method, target string, headers map[string]string, body string) {
	t.Helper()
	want := ask(t, servers[family[0]], method, target, headers, body)
	for _, name := range family[1:] {
		if got := ask(t, servers[name], method, target, headers, body); got != want {
			t.Errorf("%s answered %+v, %s answered %+v", name, got, family[0], want)
		}
	}
}

// TestEquivalence asserts literal answers for the cases it lists. These are
// the request shapes it did not list, and three of them had two answers:
// which is what a hand-written copy of a handler does when nothing compares
// it with the original. Here the claim is only that a family agrees with
// itself, so a probe needs no expected answer to be worth adding.
func TestHandlersOfOneFamilyAgree(t *testing.T) {
	json := map[string]string{"Content-Type": "application/json"}
	preflight := map[string]string{"GraphQL-Require-Preflight": "1"}
	with := func(base map[string]string, k, v string) map[string]string {
		out := map[string]string{k: v}
		for bk, bv := range base {
			out[bk] = bv
		}
		return out
	}
	const q = `{"query":"{ hello }"}`

	for _, tc := range []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		body    string
	}{
		{"GET, well formed", http.MethodGet, "/graphql?query=%7Bhello%7D", preflight, ""},
		{"GET, invalid percent-encoding", http.MethodGet, "/graphql?query=%7Bhello%7D%zz", preflight, ""},
		{"GET, query given twice", http.MethodGet, "/graphql?query=%7Bhello%7D&query=%7Bnope%7D", preflight, ""},
		{"GET, variables are not JSON", http.MethodGet, "/graphql?query=%7Bhello%7D&variables=nope", preflight, ""},
		{"GET, variables are a JSON array", http.MethodGet, "/graphql?query=%7Bhello%7D&variables=%5B1%5D", preflight, ""},
		{"GET, operationName names nothing", http.MethodGet, "/graphql?query=%7Bhello%7D&operationName=Nope", preflight, ""},
		{"GET, empty query", http.MethodGet, "/graphql?query=", preflight, ""},
		{"GET, no parameters", http.MethodGet, "/graphql", preflight, ""},
		{"PUT", http.MethodPut, "/graphql", json, q},
		{"DELETE", http.MethodDelete, "/graphql", json, q},
		{"PATCH", http.MethodPatch, "/graphql", json, q},
		{"OPTIONS", http.MethodOptions, "/graphql", json, ""},
		{"POST, empty body", http.MethodPost, "/graphql", json, ""},
		{"POST, body is null", http.MethodPost, "/graphql", json, "null"},
		{"POST, body is an array with batching off", http.MethodPost, "/graphql", json, "[" + q + "]"},
		{"POST, body is a string", http.MethodPost, "/graphql", json, `"x"`},
		{"POST, trailing garbage", http.MethodPost, "/graphql", json, q + "x"},
		{"POST, query is a number", http.MethodPost, "/graphql", json, `{"query":5}`},
		{"POST, variables are a string", http.MethodPost, "/graphql", json, `{"query":"{ hello }","variables":"x"}`},
		{"POST, charset parameter", http.MethodPost, "/graphql", map[string]string{"Content-Type": "application/json; charset=utf-8"}, q},
		{"POST, upper-case media type", http.MethodPost, "/graphql", map[string]string{"Content-Type": "APPLICATION/JSON"}, q},
		{"POST, unknown media type", http.MethodPost, "/graphql", with(preflight, "Content-Type", "application/xml"), q},
		{"POST, no content type but a preflight header", http.MethodPost, "/graphql", preflight, q},
		{"Accept refuses json outright", http.MethodPost, "/graphql", with(json, "Accept", "application/json;q=0, */*"), q},
		{"Accept refuses everything", http.MethodPost, "/graphql", with(json, "Accept", "*/*;q=0"), q},
		{"Accept refuses the stream", http.MethodPost, "/graphql", with(json, "Accept", "text/event-stream;q=0"), q},
		{"Accept is the stream with a quality", http.MethodPost, "/graphql", with(json, "Accept", "text/event-stream;q=0.5"), q},
		{"Accept is the spec type", http.MethodPost, "/graphql", with(json, "Accept", "application/graphql-response+json"), q},
		{"a validation error under the spec type", http.MethodPost, "/graphql", with(json, "Accept", "application/graphql-response+json"), `{"query":"{ nope }"}`},
		{"a parse error", http.MethodPost, "/graphql", json, `{"query":"{"}`},
		{"a subscription sent to a field that is not one", http.MethodPost, "/graphql", json, `{"query":"subscription { hello }"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers := newEquivServersWithExecutor(t, newEquivExecutor)
			agree(t, servers, httpFamily, tc.method, tc.target, tc.headers, tc.body)
			agree(t, servers, sseFamily, tc.method, tc.target, tc.headers, tc.body)
		})
	}
}

// Batching exists on gqlhttp (and so gqlecho) and on gqlfiber, written twice.
func TestBatchingHandlersAgree(t *testing.T) {
	json := map[string]string{"Content-Type": "application/json"}
	const q = `{"query":"{ hello }"}`
	for _, tc := range []struct{ name, body string }{
		{"two entries", "[" + q + "," + q + "]"},
		{"one entry", "[" + q + "]"},
		{"no entries", "[]"},
		{"past the limit", "[" + q + "," + q + "," + q + "]"},
		{"an entry is not an object", "[" + q + ",5]"},
		{"an entry has no query", "[" + q + ",{}]"},
		{"an entry is a mutation", "[" + q + `,{"query":"mutation { bump }"}]`},
		{"an entry fails validation", "[" + q + `,{"query":"{ nope }"}]`},
		{"an array inside the array", "[[" + q + "]]"},
		{"unterminated", "[" + q},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers := newEquivServers(t,
				[]gqlhttp.Option{gqlhttp.WithBatching(2)}, nil, []gqlfiber.Option{gqlfiber.WithBatching(2)})
			agree(t, servers, httpFamily, http.MethodPost, "/graphql", json, tc.body)
		})
	}
}
