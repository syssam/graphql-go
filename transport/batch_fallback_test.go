package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

// A single response that cannot be serialized is answered with a fallback
// body, because the status is already on the wire and an empty 200 reads as
// success. A batch had no such care: the array was already open when one
// entry failed, so the client got `[{...},` under a 200 -- not JSON at all,
// and with it lost the entries that had succeeded.
func TestABatchEntryThatCannotBeSerializedDoesNotTruncateTheBatch(t *testing.T) {
	mk := func() *graphql.Executor {
		s, err := graphql.NewSchema(graphql.SDL(`type Query { hello: String! }`),
			graphql.Query(graphql.Field("hello", func(graphql.Root) string { return "world" })))
		if err != nil {
			t.Fatalf("NewSchema: %v", err)
		}
		return graphql.NewExecutor(s, graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(
			func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
				if oc.OperationName == "Bad" {
					oc.SetExtension("bad", func() {})
				}
				return next(ctx, oc)
			})))
	}
	httpSrv := httptest.NewServer(gqlhttp.New(mk(), gqlhttp.WithBatching(5)))
	t.Cleanup(httpSrv.Close)
	app := fiber.New()
	app.All("/graphql", gqlfiber.New(mk(), gqlfiber.WithBatching(5)))
	servers := map[string]string{"gqlhttp": httpSrv.URL + "/graphql", "gqlfiber": startFiberEquiv(t, app) + "/graphql"}

	const body = `[{"query":"{ hello }"},{"query":"query Bad { hello }","operationName":"Bad"},{"query":"{ hello }"}]`
	for name, url := range servers {
		t.Run(name, func(t *testing.T) {
			got := ask(t, url, http.MethodPost, "", map[string]string{"Content-Type": "application/json"}, body)
			var entries []struct {
				Data   json.RawMessage `json:"data"`
				Errors []struct{ Message string }
			}
			if err := json.Unmarshal([]byte(got.body), &entries); err != nil {
				t.Fatalf("the batch response is not JSON: %v\n%s", err, got.body)
			}
			if len(entries) != 3 {
				t.Fatalf("%d entries for a batch of 3: %s", len(entries), got.body)
			}
			for _, i := range []int{0, 2} {
				if string(entries[i].Data) != `{"hello":"world"}` {
					t.Errorf("entry %d = %s, want the result it produced", i, entries[i].Data)
				}
			}
			if len(entries[1].Errors) != 1 {
				t.Errorf("the entry that could not be serialized carries %d errors, want one", len(entries[1].Errors))
			}
		})
	}
}
