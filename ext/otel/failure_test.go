package otel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/syssam/graphql-go"
	gqlotel "github.com/syssam/graphql-go/ext/otel"
)

const failureSDL = `
type Query { echo(n: Int!): Int! reject(v: String!): Int }
type Subscription { ticks: Int! broken: Int! explodes: Int! }
`

type echoArgs struct{ N int }
type rejectArgs struct{ V string }

// failureHarness is the harness over a schema whose subscription source can
// refuse to open, with room for interceptors registered after ext/otel's.
func failureHarness(t *testing.T, otelOpts []gqlotel.Option, execOpts ...graphql.ExecutorOption) *harness {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	ticks := make(chan int)
	s, err := graphql.NewSchema(graphql.SDL(failureSDL),
		graphql.Args[echoArgs](),
		graphql.Args[rejectArgs](),
		graphql.Query(
			graphql.FieldArgs("echo", func(_ graphql.Root, a echoArgs) int { return a.N }),
			// What a resolver validating its own input does: the engine's code
			// for bad input, on a message that quotes it.
			graphql.ResolveArgs("reject", func(_ context.Context, _ graphql.Root, a rejectArgs) (*int, error) {
				return nil, graphql.Errorf("%q is not a valid card number", a.V).WithCode(graphql.CodeBadUserInput)
			}),
		),
		graphql.Subscription(
			graphql.Subscribe("ticks", func(context.Context) (<-chan int, error) { return ticks, nil }),
			graphql.Subscribe("broken", func(context.Context) (<-chan int, error) {
				return nil, errors.New("source unavailable")
			}),
			graphql.Subscribe("explodes", func(context.Context) (<-chan int, error) {
				panic("source bug")
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	otelOpts = append([]gqlotel.Option{gqlotel.WithTracerProvider(tp), gqlotel.WithMeterProvider(mp)}, otelOpts...)
	opts := append(gqlotel.New(otelOpts...), execOpts...)
	return &harness{spans: sr, metrics: reader, exec: graphql.NewExecutor(s, opts...), ticks: ticks}
}

// requestMetrics sums the two request instruments, split by operation type
// ("" for a request whose type was never known).
func requestMetrics(t *testing.T, h *harness) (durations map[string]uint64, errs map[string]int64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	durations, errs = map[string]uint64{}, map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					v, _ := dp.Attributes.Value(gqlotel.AttrOperationType)
					durations[v.AsString()] += dp.Count
				}
			case metricdata.Sum[int64]:
				if m.Name != "graphql.server.errors" {
					continue
				}
				for _, dp := range d.DataPoints {
					v, _ := dp.Attributes.Value(gqlotel.AttrOperationType)
					errs[v.AsString()] += dp.Value
				}
			}
		}
	}
	return durations, errs
}

// A subscription whose source will not open never produces an event, so the
// per-event spans say nothing about it: without a span and a count of its own
// the refusal reached the client and nothing else.
func TestAFailedSubscriptionOpenIsASpanAndAnError(t *testing.T) {
	h := failureHarness(t, nil)
	_, err := h.exec.Subscribe(context.Background(), &graphql.Request{Query: `subscription Feed { broken }`})
	if err == nil {
		t.Fatal("subscribe: want the source's error")
	}

	s := named(t, h.spans.Ended(), "subscription Feed")
	if s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	if got := attrOf(t, s, gqlotel.AttrOperationType).AsString(); got != "subscription" {
		t.Fatalf("operation type = %q", got)
	}
	if got := attrOf(t, s, gqlotel.AttrErrorCount).AsInt64(); got != 1 {
		t.Fatalf("error count = %d, want 1", got)
	}
	durations, errs := requestMetrics(t, h)
	if errs["subscription"] != 1 || durations["subscription"] != 1 {
		t.Fatalf("errors = %v, durations = %v, want one of each for the subscription", errs, durations)
	}
}

// A refusal by an interceptor registered after ext/otel's is a failed open
// like any other, whatever error type the interceptor chose.
func TestASubscriptionRefusedByALaterInterceptorIsCounted(t *testing.T) {
	refuse := graphql.WithSubscriptionInterceptor(graphql.SubscriptionInterceptorFunc(
		func(context.Context, *graphql.OperationContext, graphql.SubscriptionHandler) (<-chan *graphql.Response, error) {
			return nil, errors.New("not for you")
		}))
	h := failureHarness(t, nil, refuse)
	if _, err := h.exec.Subscribe(context.Background(), &graphql.Request{Query: `subscription { ticks }`}); err == nil {
		t.Fatal("subscribe: want the refusal")
	}
	if s := named(t, h.spans.Ended(), "subscription"); s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	if _, errs := requestMetrics(t, h); errs["subscription"] != 1 {
		t.Fatalf("errors = %v, want one for the subscription", errs)
	}
}

// Executor.Subscribe recovers a panicking source above the interceptors, as
// Execute does, and answers it as a failed open.
func TestAPanicOpeningASubscriptionIsASpanAndAnError(t *testing.T) {
	h := failureHarness(t, nil)
	if _, err := h.exec.Subscribe(context.Background(), &graphql.Request{Query: `subscription { explodes }`}); err == nil {
		t.Fatal("subscribe: want the executor's recovery to have answered")
	}
	if s := named(t, h.spans.Ended(), "subscription"); s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	durations, errs := requestMetrics(t, h)
	if errs["subscription"] != 1 || durations["subscription"] != 1 {
		t.Fatalf("errors = %v, durations = %v, want one of each", errs, durations)
	}
}

// An open that succeeds stays out of the trace: its events are the spans, and
// a span for the open would put every one of them under a parent that ended
// before they began.
func TestASuccessfulSubscriptionOpenAddsNoSpan(t *testing.T) {
	h := failureHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := h.exec.Subscribe(ctx, &graphql.Request{Query: `subscription { ticks }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		h.ticks <- 1
		close(h.ticks)
	}()
	for resp := range events {
		resp.Release()
	}
	if n := len(h.spans.Ended()); n != 1 {
		t.Fatalf("got %d spans, want the one event", n)
	}
	durations, errs := requestMetrics(t, h)
	if durations["subscription"] != 1 || len(errs) != 0 {
		t.Fatalf("durations = %v, errors = %v, want the one event and no error", durations, errs)
	}
}

func panicking(what string) graphql.OperationInterceptor {
	return graphql.OperationInterceptorFunc(
		func(context.Context, *graphql.OperationContext, graphql.OperationHandler) *graphql.Response {
			panic(what)
		})
}

// The executor answers a panic outside a resolver with an internal error. It
// recovers above every interceptor, so an interceptor that records only when
// next returns records nothing for the one request an operator most needs to
// see, and its span ends looking clean.
func TestAPanicOutsideAResolverMarksTheSpanAndIsCounted(t *testing.T) {
	h := failureHarness(t, nil, graphql.WithOperationInterceptor(panicking("interceptor bug")))
	resp := h.run(t, `query Q { echo(n: 1) }`, "")
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want the executor's recovery to have answered", resp.Errors)
	}

	s := named(t, h.spans.Ended(), "query Q")
	if s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	if got := attrOf(t, s, gqlotel.AttrErrorCount).AsInt64(); got != 1 {
		t.Fatalf("error count = %d, want 1", got)
	}
	durations, errs := requestMetrics(t, h)
	if durations["query"] != 1 || errs["query"] != 1 || len(durations) != 1 || len(errs) != 1 {
		t.Fatalf("durations = %v, errors = %v, want one of each, recorded once, under the query", durations, errs)
	}
}

// Before the operation is known the request layer is the only one there is.
func TestAPanicBeforeTheOperationIsKnownIsCountedOnce(t *testing.T) {
	boom := graphql.WithRequestInterceptor(graphql.RequestInterceptorFunc(
		func(context.Context, *graphql.Request, graphql.RequestHandler) *graphql.Response {
			panic("request interceptor bug")
		}))
	h := failureHarness(t, nil, boom)
	h.run(t, `{ echo(n: 1) }`, "")

	s := named(t, h.spans.Ended(), "graphql.request")
	if s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	durations, errs := requestMetrics(t, h)
	if durations[""] != 1 || errs[""] != 1 || len(durations) != 1 || len(errs) != 1 {
		t.Fatalf("durations = %v, errors = %v, want one of each with no operation type", durations, errs)
	}
}

// A subscription event owns its span, so a panic while serving one has to be
// applied to that span by the layer that made it.
func TestAPanicServingASubscriptionEventMarksItsSpan(t *testing.T) {
	h := failureHarness(t, nil, graphql.WithOperationInterceptor(panicking("event bug")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := h.exec.Subscribe(ctx, &graphql.Request{Query: `subscription Feed { ticks }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		h.ticks <- 1
		close(h.ticks)
	}()
	for resp := range events {
		resp.Release()
	}

	s := named(t, h.spans.Ended(), "subscription Feed")
	if s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	if _, errs := requestMetrics(t, h); errs["subscription"] != 1 {
		t.Fatalf("errors = %v, want one for the event", errs)
	}
}

// An operation's name is the client's text and its length is the client's
// choice. A span name is an index key in every tracing backend, and an
// attribute is stored with every span.
func TestAnOperationNameOnASpanIsBounded(t *testing.T) {
	h := failureHarness(t, nil)
	long := strings.Repeat("A", 100_000)
	h.run(t, `query `+long+` { echo(n: 1) }`, "")

	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if !strings.HasPrefix(s.Name(), "query AAAA") || len(s.Name()) > 300 {
		t.Fatalf("span name is %d bytes: %.40q", len(s.Name()), s.Name())
	}
	if got := attrOf(t, s, gqlotel.AttrOperationName).AsString(); len(got) > 300 || !strings.HasPrefix(got, "AAAA") {
		t.Fatalf("operation name attribute is %d bytes", len(got))
	}
}

// The operationName beside a document that never parsed is not a name at
// all: over GET it is whatever bytes the query string decoded to, and an
// exporter refuses a batch holding a string that is not UTF-8.
func TestTheOperationNameOfAnUnparsedRequestIsBoundedAndValid(t *testing.T) {
	h := failureHarness(t, nil)
	h.run(t, `{ echo(n: 1)`, strings.Repeat("é", 50_000)+"\xff\xfe")
	h.run(t, `{ echo(n: 1)`, "\xff\xfe")

	for _, s := range h.spans.Ended() {
		got := attrOf(t, s, gqlotel.AttrOperationName).AsString()
		if len(got) > 300 {
			t.Fatalf("operation name attribute is %d bytes", len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("operation name attribute is not valid UTF-8: %q", got)
		}
	}
}

// spanText is everything on a span that is free text: where a value that
// should not be there would be.
func spanText(s sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	b.WriteString(s.Name() + "\n" + s.Status().Description + "\n")
	for _, kv := range s.Attributes() {
		b.WriteString(kv.Value.String() + "\n")
	}
	for _, ev := range s.Events() {
		for _, kv := range ev.Attributes {
			b.WriteString(kv.Value.String() + "\n")
		}
	}
	return b.String()
}

// The error for a variable that will not coerce quotes the value, and the one
// for a document that will not validate quotes the document. Both are what
// WithDocument is off by default to keep out of a trace.
func TestARequestErrorDoesNotPutClientTextOnTheSpan(t *testing.T) {
	const secret = "hunter2-card-4111"
	bad := &graphql.Request{
		Query:     `query Q($n: Int!) { echo(n: $n) }`,
		Variables: []byte(`{"n":"` + secret + `"}`),
	}

	h := failureHarness(t, nil)
	resp := h.exec.Execute(context.Background(), bad)
	t.Cleanup(resp.Release)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, secret) {
		t.Fatalf("errors = %v, want one quoting the variable's value", resp.Errors)
	}
	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if text := spanText(s); strings.Contains(text, secret) {
		t.Fatalf("the variable's value is on the span:\n%s", text)
	}
	if s.Status().Code != codes.Error || !strings.Contains(s.Status().Description, graphql.CodeBadUserInput) {
		t.Fatalf("status = %+v, want an error naming the code", s.Status())
	}

	on := failureHarness(t, []gqlotel.Option{gqlotel.WithDocument(true)})
	resp = on.exec.Execute(context.Background(), bad)
	t.Cleanup(resp.Release)
	if text := spanText(on.spans.Ended()[0]); !strings.Contains(text, secret) {
		t.Fatalf("WithDocument asked for the client's text and the message is missing:\n%s", text)
	}
}

// The code says the message is about the client's input wherever the error
// was raised, and with field spans on it is recorded twice: on the operation
// and on the field.
func TestAnInputErrorFromAResolverIsCodeOnlyOnEverySpan(t *testing.T) {
	const secret = "4111-1111-1111-1111"
	h := failureHarness(t, []gqlotel.Option{gqlotel.WithFieldSpans(true)})
	resp := h.exec.Execute(context.Background(), &graphql.Request{
		Query:     `query Q($v: String!) { reject(v: $v) }`,
		Variables: []byte(`{"v":"` + secret + `"}`),
	})
	t.Cleanup(resp.Release)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, secret) {
		t.Fatalf("errors = %v, want the resolver's, quoting its input", resp.Errors)
	}
	spans := h.spans.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want the operation and the field", len(spans))
	}
	for _, s := range spans {
		if s.Status().Code != codes.Error || s.Status().Description != graphql.CodeBadUserInput {
			t.Errorf("span %q: status = %+v, want an error naming the code", s.Name(), s.Status())
		}
		if text := spanText(s); strings.Contains(text, secret) {
			t.Errorf("span %q carries the input:\n%s", s.Name(), text)
		}
	}
}

// A field error's message is the server's own, and is the part of a failed
// request worth reading in a trace.
func TestAFieldErrorKeepsItsMessageOnTheSpan(t *testing.T) {
	h := newHarness(t)
	h.run(t, `{ me { boom } }`, "")
	if s := named(t, h.spans.Ended(), "query"); s.Status().Description != "boom" {
		t.Fatalf("status description = %q, want the resolver's message", s.Status().Description)
	}
}
