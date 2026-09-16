package graphql

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The shared fixture has no subscription root, and giving it one would force
// every other test's schema through subscription coverage validation. This
// file carries its own minimal schema instead, shared by all tests in it.

const subSDL = `
type Message { id: ID! body: String! fail: String! seq: Int! }
type Query { ping: String! }
type Subscription {
  messages: Message!
  maybe: Message
  counter: Int!
  countdown(from: Int!): Int!
}
`

type subMessage struct {
	ID   string
	Body string
}

type fromArgs struct{ From int }

var errSourceBoom = errors.New("boom")

// subSource holds the channels a test feeds, so a test can drive events and
// close the stream on its own schedule.
type subSource struct {
	messages chan *subMessage
	maybe    chan *subMessage
	counter  chan int
	openErr  error
	opens    atomic.Int64
}

func newSubSource() *subSource {
	return &subSource{
		messages: make(chan *subMessage),
		maybe:    make(chan *subMessage),
		counter:  make(chan int),
	}
}

func (s *subSource) options() []SchemaOption {
	return []SchemaOption{
		Object[subMessage]("Message",
			Field("id", func(m *subMessage) string { return m.ID }),
			Field("body", func(m *subMessage) string { return m.Body }),
			Resolve("fail", func(context.Context, *subMessage) (string, error) { return "", errSourceBoom }),
			// seq reaches its operation through the context, which is the only
			// way a resolver can, and records how many times it has run against
			// the context it was given. A shared context would make the second
			// event report the first event's count.
			Resolve("seq", func(ctx context.Context, _ *subMessage) (int, error) {
				oc := OperationFrom(ctx)
				if oc == nil {
					return 0, errors.New("no operation on the resolver context")
				}
				n := new(int)
				actual, _ := oc.GetOrSet("seq", n)
				*actual.(*int)++
				oc.SetExtension("seq", *actual.(*int))
				return *actual.(*int), nil
			}),
		),
		Args[fromArgs](InputField("from", func(a *fromArgs, v int) { a.From = v })),
		Query(
			Field("ping", func(Root) string { return "pong" }),
		),
		Subscription(
			Subscribe("messages", func(context.Context) (<-chan *subMessage, error) {
				s.opens.Add(1)
				if s.openErr != nil {
					return nil, s.openErr
				}
				return s.messages, nil
			}),
			Subscribe("maybe", func(context.Context) (<-chan *subMessage, error) { return s.maybe, nil }),
			Subscribe("counter", func(context.Context) (<-chan int, error) { return s.counter, nil }),
			SubscribeArgs("countdown", func(ctx context.Context, a fromArgs) (<-chan int, error) {
				ch := make(chan int)
				go func() {
					defer close(ch)
					for i := a.From; i > 0; i-- {
						select {
						case ch <- i:
						case <-ctx.Done():
							return
						}
					}
				}()
				return ch, nil
			}),
		),
	}
}

func newSubExecutor(t *testing.T, opts ...ExecutorOption) (*subSource, *Executor) {
	t.Helper()
	src := newSubSource()
	s, err := NewSchema(SDL(subSDL), src.options()...)
	if err != nil {
		t.Fatalf("subscription schema: %v", err)
	}
	return src, NewExecutor(s, opts...)
}

// nextResponse reads one response, failing rather than hanging the suite.
func nextResponse(t *testing.T, ch <-chan *Response) *Response {
	t.Helper()
	select {
	case resp, ok := <-ch:
		if !ok {
			t.Fatal("stream closed, wanted a response")
		}
		return resp
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a response")
		return nil
	}
}

// expectClosed asserts the stream ends rather than delivering more.
func expectClosed(t *testing.T, ch <-chan *Response) {
	t.Helper()
	select {
	case resp, ok := <-ch:
		if ok {
			t.Fatalf("wanted a closed stream, got a response: %s", resp.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stream to close")
	}
}

func TestSubscribeCompositeEvents(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id body } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	go func() {
		src.messages <- &subMessage{ID: "1", Body: "hello"}
		src.messages <- &subMessage{ID: "2", Body: "world"}
		close(src.messages)
	}()

	want := []string{
		`{"messages":{"id":"1","body":"hello"}}`,
		`{"messages":{"id":"2","body":"world"}}`,
	}
	for _, w := range want {
		resp := nextResponse(t, out)
		if len(resp.Errors) > 0 {
			t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
		}
		if got := string(resp.Data); got != w {
			t.Fatalf("data mismatch\n got: %s\nwant: %s", got, w)
		}
		resp.Release()
	}
	expectClosed(t, out)
}

func TestSubscribeLeafEvents(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { counter }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		src.counter <- 7
		close(src.counter)
	}()

	resp := nextResponse(t, out)
	if got := string(resp.Data); got != `{"counter":7}` {
		t.Fatalf("data mismatch: %s", got)
	}
	resp.Release()
	expectClosed(t, out)
}

func TestSubscribeArgsAndAlias(t *testing.T) {
	_, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { tick: countdown(from: 3) }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	for _, want := range []string{`{"tick":3}`, `{"tick":2}`, `{"tick":1}`} {
		resp := nextResponse(t, out)
		if got := string(resp.Data); got != want {
			t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
		}
		resp.Release()
	}
	expectClosed(t, out)
}

func TestSubscribeVariables(t *testing.T) {
	_, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{
		Query:     `subscription C($n: Int!) { countdown(from: $n) }`,
		Variables: []byte(`{"n":2}`),
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	for _, want := range []string{`{"countdown":2}`, `{"countdown":1}`} {
		resp := nextResponse(t, out)
		if got := string(resp.Data); got != want {
			t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
		}
		resp.Release()
	}
	expectClosed(t, out)
}

// TestSubscribeCancelStopsStream is the unsubscribe path: cancelling the
// context must close the response channel without the source cooperating.
func TestSubscribeCancelStopsStream(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() { src.messages <- &subMessage{ID: "1"} }()

	resp := nextResponse(t, out)
	resp.Release()

	cancel()
	expectClosed(t, out)
}

// TestSubscribeEventErrorsBubble checks that per-event execution keeps the
// ordinary null-bubbling contract: a failing non-null field nulls the whole
// event payload rather than the subscription ending.
func TestSubscribeEventErrorsBubble(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id fail } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		src.messages <- &subMessage{ID: "1"}
		src.messages <- &subMessage{ID: "2"}
		close(src.messages)
	}()

	for range 2 {
		resp := nextResponse(t, out)
		if got := string(resp.Data); got != "null" {
			t.Fatalf("wanted the non-null violation to bubble to data null, got %s", got)
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("wanted one error, got %s", errorsJSON(resp.Errors))
		}
		if got := resp.Errors[0].Path.String(); got != "messages.fail" {
			t.Fatalf("error path = %q, want messages.fail", got)
		}
		resp.Release()
	}
	expectClosed(t, out)
}

// TestSubscribeNullableEventNulls covers a nullable root field: a nil event
// writes null for that key and the subscription continues.
func TestSubscribeNullableEventNulls(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { maybe { id } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		src.maybe <- nil
		src.maybe <- &subMessage{ID: "1"}
		close(src.maybe)
	}()

	for _, want := range []string{`{"maybe":null}`, `{"maybe":{"id":"1"}}`} {
		resp := nextResponse(t, out)
		if len(resp.Errors) > 0 {
			t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
		}
		if got := string(resp.Data); got != want {
			t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
		}
		resp.Release()
	}
	expectClosed(t, out)
}

// TestSubscribeFieldObserverSeesRootFieldPerEvent pins FINDING-9's ruling: a
// FieldObserver runs in callLeaf/callResolve before dispatch to f.exec, so
// substituting f.exec per event (see the comment on runSubscriptionEvent)
// does not hide the root field from it the way it hides the root field from
// a FieldInterceptor. The observer must see the root field once per event,
// not once for the whole subscription.
func TestSubscribeFieldObserverSeesRootFieldPerEvent(t *testing.T) {
	o := &recordingObserver{}
	src, e := newSubExecutor(t, WithFieldObserver(o))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id body } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	go func() {
		src.messages <- &subMessage{ID: "1", Body: "hello"}
		src.messages <- &subMessage{ID: "2", Body: "world"}
		close(src.messages)
	}()

	for range 2 {
		resp := nextResponse(t, out)
		if len(resp.Errors) > 0 {
			t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
		}
		resp.Release()
	}
	expectClosed(t, out)

	var rootHits int
	for _, name := range o.begun {
		if name == "Subscription.messages" {
			rootHits++
		}
	}
	if rootHits != 2 {
		t.Fatalf("observer saw the root field %d times, want 2 (once per event); saw %v", rootHits, o.begun)
	}
}

// TestSubscribeOperationInterceptorPerEvent asserts every event goes through
// the operation chain with its own context. That is what lets tracing and
// limits treat an event like an operation, and it is the observable proof
// that extension state such as a DataLoader cache cannot leak between events.
func TestSubscribeOperationInterceptorPerEvent(t *testing.T) {
	src := newSubSource()
	s, err := NewSchema(SDL(subSDL), src.options()...)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	var events atomic.Int64
	contexts := make(chan *OperationContext, 4)
	e := NewExecutor(s, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			events.Add(1)
			contexts <- oc
			oc.Set("seen", true)
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

	for range 2 {
		nextResponse(t, out).Release()
	}
	expectClosed(t, out)

	if n := events.Load(); n != 2 {
		t.Fatalf("operation interceptor ran %d times, want 2", n)
	}
	close(contexts)
	var got []*OperationContext
	for oc := range contexts {
		got = append(got, oc)
	}
	if got[0] == got[1] {
		t.Fatal("both events shared one OperationContext; per-event state would leak")
	}
	if _, ok := got[1].Get("seen"); !ok {
		t.Fatal("the second event's own value was not stored")
	}
}

func TestSubscribeRequestErrors(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx := context.Background()

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"parse", `subscription {`, "Expected Name"},
		{"validation", `subscription { nope }`, `Cannot query field "nope"`},
		{"not a subscription", `{ ping }`, "Subscribe requires a subscription operation"},
		{"two root fields", `subscription { counter messages { id } }`, "must select only one top level field"},
		// The validator counts selections, so @skip leaving none is caught
		// only after the plan variant is folded.
		{"skipped root field", `subscription { counter @skip(if: true) }`, "must select exactly one root field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.Subscribe(ctx, &Request{Query: tc.query})
			if out != nil {
				t.Fatal("wanted no stream")
			}
			var se *SubscribeError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not a *SubscribeError", err)
			}
			if se.Response == nil || len(se.Response.Errors) == 0 {
				t.Fatal("wanted a response carrying errors")
			}
			if msg := se.Response.Errors[0].Message; !strings.Contains(msg, tc.want) {
				t.Fatalf("message %q does not contain %q", msg, tc.want)
			}
		})
	}

	t.Run("open error", func(t *testing.T) {
		src.openErr = errSourceBoom
		out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id } }`})
		if out != nil {
			t.Fatal("wanted no stream")
		}
		var se *SubscribeError
		if !errors.As(err, &se) {
			t.Fatalf("error %v is not a *SubscribeError", err)
		}
		if got := se.Response.Errors[0].Path.String(); got != "messages" {
			t.Fatalf("error path = %q, want messages", got)
		}
	})
}

// TestExecuteRejectsSubscription keeps the request/response transports honest:
// a subscription over Execute is a request error, not a hang.
func TestExecuteRejectsSubscription(t *testing.T) {
	_, e := newSubExecutor(t)
	resp := e.Execute(context.Background(), &Request{Query: `subscription { counter }`})
	if !resp.HasRequestErrors() {
		t.Fatalf("wanted a request error, got %s", resp.Data)
	}
	if msg := resp.Errors[0].Message; !strings.Contains(msg, "Subscribe") {
		t.Fatalf("message %q should point at Subscribe", msg)
	}
}

func TestSubscribeBindingErrors(t *testing.T) {
	tests := []struct {
		name string
		opts []SchemaOption
		want string
	}{
		{
			name: "resolver on the subscription root",
			opts: []SchemaOption{
				Object[subMessage]("Message",
					Field("id", func(m *subMessage) string { return m.ID }),
					Field("body", func(m *subMessage) string { return m.Body }),
					Resolve("fail", func(context.Context, *subMessage) (string, error) { return "", errSourceBoom }),
				),
				Args[fromArgs](InputField("from", func(a *fromArgs, v int) { a.From = v })),
				Query(Field("ping", func(Root) string { return "pong" })),
				Subscription(
					Resolve("messages", func(context.Context, Root) (*subMessage, error) { return nil, nil }),
					Subscribe("maybe", func(context.Context) (<-chan *subMessage, error) { return nil, nil }),
					Subscribe("counter", func(context.Context) (<-chan int, error) { return nil, nil }),
					SubscribeArgs("countdown", func(context.Context, fromArgs) (<-chan int, error) { return nil, nil }),
				),
			},
			want: "Subscription.messages must be bound with Subscribe",
		},
		{
			name: "subscribe outside the subscription root",
			opts: []SchemaOption{
				Object[subMessage]("Message",
					Field("id", func(m *subMessage) string { return m.ID }),
					Field("body", func(m *subMessage) string { return m.Body }),
					Resolve("fail", func(context.Context, *subMessage) (string, error) { return "", errSourceBoom }),
				),
				Args[fromArgs](InputField("from", func(a *fromArgs, v int) { a.From = v })),
				Query(Subscribe("ping", func(context.Context) (<-chan string, error) { return nil, nil })),
				Subscription(
					Subscribe("messages", func(context.Context) (<-chan *subMessage, error) { return nil, nil }),
					Subscribe("maybe", func(context.Context) (<-chan *subMessage, error) { return nil, nil }),
					Subscribe("counter", func(context.Context) (<-chan int, error) { return nil, nil }),
					SubscribeArgs("countdown", func(context.Context, fromArgs) (<-chan int, error) { return nil, nil }),
				),
			},
			want: "Subscribe binds fields of the subscription root only",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(SDL(subSDL), tc.opts...)
			if err == nil {
				t.Fatal("wanted a schema error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestSubscribeEventStateIsPerEvent is the guard on the whole per-event
// isolation claim. A resolver reaches its operation through
// OperationFrom(ctx), so installing the event's context only in the chain
// argument and not in ctx would leave every event sharing the base: one
// DataLoader cache for the life of the subscription, no wave coordinator to
// batch into, and extensions written where no response reads them.
func TestSubscribeEventStateIsPerEvent(t *testing.T) {
	src, e := newSubExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id seq } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		src.messages <- &subMessage{ID: "1"}
		src.messages <- &subMessage{ID: "2"}
		close(src.messages)
	}()

	for _, want := range []string{`{"messages":{"id":"1","seq":1}}`, `{"messages":{"id":"2","seq":1}}`} {
		resp := nextResponse(t, out)
		if len(resp.Errors) > 0 {
			t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
		}
		if got := string(resp.Data); got != want {
			t.Fatalf("data mismatch\n got: %s\nwant: %s\n(a count above 1 means the events shared one operation context)", got, want)
		}
		// Extensions are cloned from the event's own context, so a resolver
		// writing to a different one would leave this empty.
		if got, ok := resp.Extensions["seq"]; !ok || got != 1 {
			t.Fatalf("extensions = %v, want seq 1 from this event", resp.Extensions)
		}
		resp.Release()
	}
	expectClosed(t, out)
}
