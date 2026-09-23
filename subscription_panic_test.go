package graphql

import (
	"context"
	"sync/atomic"
	"testing"
)

// Each event runs the operation interceptors, the presenter and the leaf
// writers on pump's goroutine. A panic there reached no recover: in Execute
// the transport's would catch it, but on pump's goroutine it ended the
// process. It must cost one event, not the stream or the server.
func TestSubscribeEventPanicIsAnErrorEvent(t *testing.T) {
	var events atomic.Int64
	src, e := newSubExecutor(t, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			if events.Add(1) == 1 {
				panic("interceptor boom")
			}
			return next(ctx, oc)
		})))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, serr := e.Subscribe(ctx, &Request{Query: `subscription { messages { id } }`})
	if serr != nil {
		t.Fatalf("subscribe: %v", serr)
	}
	go func() {
		src.messages <- &subMessage{ID: "1"}
		src.messages <- &subMessage{ID: "2"}
		close(src.messages)
	}()

	first := nextResponse(t, out)
	if first.Data != nil || len(first.Errors) != 1 || first.Errors[0].Extensions["code"] != CodeInternal {
		t.Fatalf("first event = data %s errors %s, want one INTERNAL_SERVER_ERROR", first.Data, errorsJSON(first.Errors))
	}
	if msg := first.Errors[0].Message; msg != "internal system error" {
		t.Fatalf("message = %q: the panic value must not reach the client", msg)
	}
	expectData(t, nextResponse(t, out), `{"messages":{"id":"2"}}`)
	expectClosed(t, out)
}
