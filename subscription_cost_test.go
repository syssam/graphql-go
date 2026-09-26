package graphql

import (
	"context"
	"errors"
	"testing"
)

// Depth and complexity refuse a subscription before its plan is compiled;
// cost depends on the variables and used to be checked only per event, so an
// over-cost subscription opened its source and then answered every event with
// an error.
func TestSubscribeRefusesAnOverCostOperationBeforeTheSourceOpens(t *testing.T) {
	src, e := newSubExecutor(t, WithQueryCost(QueryCost{Max: 1}))
	_, err := e.Subscribe(context.Background(), &Request{Query: `subscription { messages { id body } }`})
	var se *SubscribeError
	if !errors.As(err, &se) || len(se.Response.Errors) == 0 || se.Response.Errors[0].Extensions["maxQueryCost"] != 1 {
		t.Fatalf("Subscribe error = %v, want the cost limit", err)
	}
	if n := src.opens.Load(); n != 0 {
		t.Errorf("source opened %d times for a refused subscription", n)
	}
}

// Every event runs the operation chain with its own OperationContext, and a
// rate limiter reads oc.Cost() there. It must be the configured model's
// number, not the no-model fallback.
func TestSubscriptionEventCostUsesTheConfiguredModel(t *testing.T) {
	seen := make(chan int, 1)
	src, e := newSubExecutor(t,
		WithQueryCost(QueryCost{FieldWeight: map[string]int{"Message.body": 100}}),
		WithOperationInterceptor(OperationInterceptorFunc(
			func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
				seen <- oc.Cost()
				return next(ctx, oc)
			})))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id body } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() { src.messages <- &subMessage{ID: "1", Body: "b"} }()
	nextResponse(t, out).Release()
	// messages 1 + id 1 + body 100.
	if got := <-seen; got != 102 {
		t.Errorf("cost seen for an event = %d, want 102", got)
	}
}
