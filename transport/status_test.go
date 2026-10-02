package transport_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/syssam/graphql-go"
)

// A response with errors and no data is a request error, and under the
// specification's media type a request error is 400: the client sent
// something the server would not run. A panic, or a policy backend that could
// not be reached, also leaves no data, and is the server's failure. Answered
// 400 it never reached 5xx alerting, and told a client not to retry.
func TestAServerFailureIsNotAnsweredAsAClientError(t *testing.T) {
	servers := newEquivServersWithExecutor(t, func(t *testing.T) *graphql.Executor {
		t.Helper()
		s, err := graphql.NewSchema(graphql.SDL(`type Query { hello: String! }`),
			graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })))
		if err != nil {
			t.Fatalf("NewSchema: %v", err)
		}
		return graphql.NewExecutor(s, graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(
			func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
				if oc.OperationName == "Boom" {
					panic("interceptor boom")
				}
				return next(ctx, oc)
			})))
	})
	spec := map[string]string{"Content-Type": "application/json", "Accept": "application/graphql-response+json"}
	stream := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"}
	const boom = `{"query":"query Boom { hello }","operationName":"Boom"}`
	const invalid = `{"query":"{ nope }"}`

	for _, tc := range []struct {
		name    string
		family  []string
		headers map[string]string
		body    string
		want    int
	}{
		{"a panic, spec media type", httpFamily, spec, boom, http.StatusInternalServerError},
		{"a panic, on a stream endpoint", sseFamily, stream, boom, http.StatusInternalServerError},
		{"a validation error, spec media type", httpFamily, spec, invalid, http.StatusBadRequest},
		{"a validation error, on a stream endpoint", sseFamily, stream, invalid, http.StatusBadRequest},
		// application/json promises 200 for anything that is a GraphQL response.
		{"a panic, application/json", httpFamily, map[string]string{"Content-Type": "application/json"}, boom, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range tc.family {
				if got := ask(t, servers[name], http.MethodPost, "/graphql", tc.headers, tc.body); got.status != tc.want {
					t.Errorf("%s answered %d, want %d: %s", name, got.status, tc.want, got.body)
				}
			}
		})
	}
}
