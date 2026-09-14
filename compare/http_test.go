package compare_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/compare"
	theirgen "github.com/syssam/graphql-go/compare/gqlgen"
	"github.com/syssam/graphql-go/compare/gqlgen/exec"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

// These benchmarks answer a question the in-process ones cannot: how much of
// the engine difference survives once a request has to arrive over HTTP.
// Transport cost is a fixed charge added to both engines, so a ratio measured
// in-process is an upper bound on what a server actually delivers.

func newGraphQLGoServer(tb testing.TB) (*httptest.Server, *http.Client) {
	tb.Helper()
	e, err := compare.NewGraphQLGo()
	if err != nil {
		tb.Fatal(err)
	}
	srv := httptest.NewServer(gqlhttp.New(e))
	tb.Cleanup(srv.Close)
	return srv, srv.Client()
}

func newGqlgenServer(tb testing.TB) (*httptest.Server, *http.Client) {
	tb.Helper()
	es := exec.NewExecutableSchema(exec.Config{Resolvers: &theirgen.Resolver{}})
	srv := httptest.NewServer(handler.NewDefaultServer(es))
	tb.Cleanup(srv.Close)
	return srv, srv.Client()
}

func postGraphQL(tb testing.TB, c *http.Client, url, query string) []byte {
	tb.Helper()
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		tb.Fatal(err)
	}
	resp, err := c.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		tb.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		tb.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if bytes.Contains(out, []byte(`"errors"`)) {
		tb.Fatalf("errors in response: %s", out)
	}
	return out
}

func benchOverHTTP(b *testing.B, newServer func(testing.TB) (*httptest.Server, *http.Client)) {
	srv, c := newServer(b)
	for _, q := range benchQueries {
		if q.name == "Introspection" {
			continue // the servers differ in what they expose; not comparable here
		}
		b.Run(q.name, func(b *testing.B) {
			postGraphQL(b, c, srv.URL, q.query)
			b.ReportAllocs()
			for b.Loop() {
				postGraphQL(b, c, srv.URL, q.query)
			}
		})
	}
}

func BenchmarkHTTPGraphQLGo(b *testing.B) { benchOverHTTP(b, newGraphQLGoServer) }
func BenchmarkHTTPGqlgen(b *testing.B)    { benchOverHTTP(b, newGqlgenServer) }

// TestHTTPEnginesAgree keeps the HTTP benchmarks honest: both servers must
// still return the same data once the transport is in the path.
func TestHTTPEnginesAgree(t *testing.T) {
	ourSrv, ourClient := newGraphQLGoServer(t)
	theirSrv, theirClient := newGqlgenServer(t)

	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			a := extractData(t, postGraphQL(t, ourClient, ourSrv.URL, q))
			b := extractData(t, postGraphQL(t, theirClient, theirSrv.URL, q))
			if a != b {
				t.Errorf("engines disagree over HTTP\ngraphql-go: %s\ngqlgen:     %s", a, b)
			}
		})
	}
}

func extractData(t *testing.T, raw []byte) string {
	t.Helper()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return normalise(t, envelope.Data)
}

var _ = graphql.ID("")
