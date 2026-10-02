package gqlws_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/syssam/graphql-go"
)

// Each operation runs on a goroutine of its own, with no net/http recover
// above it. A panic the executor did not turn into a response therefore ended
// the process, and with it every other connection, for one client's request.
func TestPanicInAnOperationCostsThatOperationOnly(t *testing.T) {
	var calls atomic.Int64
	s, err := graphql.NewSchema(graphql.SDL(`type Query { ping: String! }`),
		graphql.Query(graphql.Field("ping", func(graphql.Root) string { return "pong" })))
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	e := graphql.NewExecutor(s, graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(
		func(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
			if calls.Add(1) == 1 {
				panic("interceptor boom")
			}
			return next(ctx, oc)
		})))
	c := dial(t, e)
	c.init("")

	c.subscribe("1", `{ ping }`)
	got := c.recv()
	if got.Type != "error" || got.ID != "1" {
		t.Fatalf("frame = %+v, want a terminal error for the operation that panicked", got)
	}
	var errs []struct{ Message string }
	if err := json.Unmarshal(got.Payload, &errs); err != nil || len(errs) != 1 || errs[0].Message != "internal system error" {
		t.Fatalf("error payload = %s, want one internal system error", got.Payload)
	}

	c.subscribe("2", `{ ping }`)
	if got := c.recv(); got.Type != "next" || got.ID != "2" || string(got.Payload) != `{"data":{"ping":"pong"}}` {
		t.Fatalf("the connection did not survive: %+v", got)
	}
}
