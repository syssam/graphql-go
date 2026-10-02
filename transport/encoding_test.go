package transport_test

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"testing"
)

// The net/http handlers never decompress a request body: compression is the
// surrounding server's business. gqlfiber read its body through Fiber's
// Body(), which does, so the same request had two answers -- and when the
// decode failed Fiber returned its error text as the body, which gqlfiber
// then parsed as the request and answered with Fiber's message glued to the
// front of a GraphQL envelope.
func TestARequestBodyIsNeverDecompressed(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte(`{"query":"{ hello }"}`))
	_ = zw.Close()

	for _, tc := range []struct{ name, encoding, body string }{
		{"gzip body", "gzip", zipped.String()},
		{"an encoding nobody implements, plain body", "compress", `{"query":"{ hello }"}`},
		{"an unknown encoding, plain body", "nonsense", `{"query":"{ hello }"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers := newEquivServersWithExecutor(t, newEquivExecutor)
			headers := map[string]string{"Content-Type": "application/json", "Content-Encoding": tc.encoding}
			for _, family := range [][]string{httpFamily, sseFamily} {
				wantStatus, wantBody := doRequest(t, servers[family[0]], http.MethodPost, "/graphql", headers, tc.body)
				for _, name := range family[1:] {
					status, body := doRequest(t, servers[name], http.MethodPost, "/graphql", headers, tc.body)
					if status != wantStatus || body != wantBody {
						t.Errorf("%s answered %d %q, %s answered %d %q", name, status, body, family[0], wantStatus, wantBody)
					}
				}
			}
		})
	}
}
