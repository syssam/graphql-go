package otel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/syssam/graphql-go"
	gqlotel "github.com/syssam/graphql-go/ext/otel"
)

const sdl = `
type User { id: ID! name: String! boom: String! }
type Query { me: User! }
type Subscription { ticks: Int! }
`

type user struct {
	ID   string
	Name string
}

type harness struct {
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
	exec    *graphql.Executor
	ticks   chan int
}

func newHarness(t *testing.T, opts ...gqlotel.Option) *harness {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	ticks := make(chan int)
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) string { return u.ID }),
			graphql.Field("name", func(u *user) string { return u.Name }),
			graphql.Resolve("boom", func(context.Context, *user) (string, error) {
				return "", errors.New("boom")
			}),
		),
		graphql.Query(
			graphql.Resolve("me", func(context.Context, graphql.Root) (*user, error) {
				return &user{ID: "1", Name: "Ada"}, nil
			}),
		),
		graphql.Subscription(
			graphql.Subscribe("ticks", func(context.Context) (<-chan int, error) { return ticks, nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	opts = append([]gqlotel.Option{
		gqlotel.WithTracerProvider(tp),
		gqlotel.WithMeterProvider(mp),
	}, opts...)
	return &harness{
		spans:   sr,
		metrics: reader,
		exec:    graphql.NewExecutor(s, gqlotel.New(opts...)...),
		ticks:   ticks,
	}
}

func (h *harness) run(t *testing.T, query, opName string) *graphql.Response {
	t.Helper()
	resp := h.exec.Execute(context.Background(), &graphql.Request{Query: query, OperationName: opName})
	t.Cleanup(resp.Release)
	return resp
}

func attrOf(t *testing.T, s sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	t.Helper()
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no attribute %s (has %v)", s.Name(), key, s.Attributes())
	return attribute.Value{}
}

// named returns the one span with this name, failing if there is not exactly
// one. Field spans make several, so tests that enable them look up by name.
func named(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			if found != nil {
				t.Fatalf("more than one span named %q", name)
			}
			found = s
		}
	}
	if found == nil {
		var names []string
		for _, s := range spans {
			names = append(names, s.Name())
		}
		t.Fatalf("no span named %q, got %v", name, names)
	}
	return found
}

// TestQuerySpanIsNamedForTheOperation covers the reason the span is renamed
// rather than named up front: the name only exists after parsing, and a span
// called graphql.request tells an operator nothing.
func TestQuerySpanIsNamedForTheOperation(t *testing.T) {
	h := newHarness(t)
	h.run(t, `query UserPage { me { id name } }`, "")

	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "query UserPage" {
		t.Fatalf("span name = %q", s.Name())
	}
	if got := attrOf(t, s, gqlotel.AttrOperationType).AsString(); got != "query" {
		t.Fatalf("operation type = %q", got)
	}
	if got := attrOf(t, s, gqlotel.AttrOperationName).AsString(); got != "UserPage" {
		t.Fatalf("operation name = %q", got)
	}
	if got := attrOf(t, s, gqlotel.AttrErrorCount).AsInt64(); got != 0 {
		t.Fatalf("error count = %d", got)
	}
	if s.Status().Code != codes.Ok {
		t.Fatalf("status = %v", s.Status())
	}
	if attrOf(t, s, gqlotel.AttrComplexity).AsInt64() == 0 {
		t.Fatal("complexity should come from the compiled plan")
	}
}

func TestAnonymousOperationHasNoTrailingSpace(t *testing.T) {
	h := newHarness(t)
	h.run(t, `{ me { id } }`, "")
	if got := named(t, h.spans.Ended(), "query").Name(); got != "query" {
		t.Fatalf("span name = %q", got)
	}
}

// TestParseFailureStillProducesASpan is why the span starts in the request
// interceptor: a document that never parses never reaches the operation
// chain, and a failure with no span is the one an operator most wants.
func TestParseFailureStillProducesASpan(t *testing.T) {
	h := newHarness(t)
	resp := h.run(t, `{ me {`, "")
	if !resp.HasRequestErrors() {
		t.Fatal("wanted a request error")
	}

	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "graphql.request" {
		t.Fatalf("span name = %q, want the pre-parse name", s.Name())
	}
	if s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	if len(s.Events()) == 0 {
		t.Fatal("the error should be recorded on the span")
	}
}

// TestFieldErrorsMarkTheSpan covers the case a status code alone would hide:
// the response is a 200 carrying errors, but the request did not do what was
// asked.
func TestFieldErrorsMarkTheSpan(t *testing.T) {
	h := newHarness(t)
	resp := h.run(t, `{ me { boom } }`, "")
	if len(resp.Errors) != 1 {
		t.Fatalf("wanted one error, got %d", len(resp.Errors))
	}

	s := named(t, h.spans.Ended(), "query")
	if s.Status().Code != codes.Error {
		t.Fatalf("status = %v, want error", s.Status())
	}
	if got := attrOf(t, s, gqlotel.AttrErrorCount).AsInt64(); got != 1 {
		t.Fatalf("error count = %d", got)
	}
}

func TestFieldSpansAreOptional(t *testing.T) {
	off := newHarness(t)
	off.run(t, `{ me { id name } }`, "")
	if n := len(off.spans.Ended()); n != 1 {
		t.Fatalf("got %d spans with field spans off, want 1", n)
	}

	on := newHarness(t, gqlotel.WithFieldSpans(true))
	on.run(t, `{ me { id name } }`, "")
	spans := on.spans.Ended()
	if len(spans) < 4 {
		t.Fatalf("got %d spans, want the operation plus one per field", len(spans))
	}
	field := named(t, spans, "User.name")
	if got := attrOf(t, field, gqlotel.AttrFieldPath).AsString(); got != "me.name" {
		t.Fatalf("field path = %q", got)
	}
}

func TestDocumentIsNotRecordedByDefault(t *testing.T) {
	const query = `{ me { id } }`

	off := newHarness(t)
	off.run(t, query, "")
	for _, kv := range named(t, off.spans.Ended(), "query").Attributes() {
		if kv.Key == gqlotel.AttrDocument {
			t.Fatal("the document should not be recorded unless asked for")
		}
	}

	on := newHarness(t, gqlotel.WithDocument(true))
	on.run(t, query, "")
	if got := attrOf(t, named(t, on.spans.Ended(), "query"), gqlotel.AttrDocument).AsString(); got != query {
		t.Fatalf("document = %q", got)
	}
}

// TestSubscriptionEventsGetTheirOwnSpans is the documented exception to one
// span per request: Subscribe skips the request chain, so each event is its
// own unit of work rather than a subscription open for a day being one span.
func TestSubscriptionEventsGetTheirOwnSpans(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := h.exec.Subscribe(ctx, &graphql.Request{Query: `subscription Feed { ticks }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		h.ticks <- 1
		h.ticks <- 2
		close(h.ticks)
	}()
	for range 2 {
		resp, ok := <-events
		if !ok {
			t.Fatal("stream closed early")
		}
		resp.Release()
	}
	for range events {
	}

	spans := h.spans.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want one per event", len(spans))
	}
	for _, s := range spans {
		if s.Name() != "subscription Feed" {
			t.Fatalf("span name = %q", s.Name())
		}
		if s.Status().Code != codes.Ok {
			t.Fatalf("status = %v", s.Status())
		}
	}
}

func TestMetricsAreRecorded(t *testing.T) {
	h := newHarness(t)
	h.run(t, `{ me { id } }`, "")
	h.run(t, `{ me { boom } }`, "")

	var rm metricdata.ResourceMetrics
	if err := h.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	var sawDuration, sawErrors bool
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != gqlotel.ScopeName {
			continue
		}
		for _, m := range sm.Metrics {
			switch m.Name {
			case "graphql.server.request.duration":
				sawDuration = true
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration is %T, want a float histogram", m.Data)
				}
				var count uint64
				for _, dp := range h.DataPoints {
					count += dp.Count
				}
				// Exactly one record per request, not one per chain layer.
				if count != 2 {
					t.Fatalf("duration recorded %d times for 2 requests, want 2", count)
				}
			case "graphql.server.errors":
				sawErrors = true
				c, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("errors is %T, want an int sum", m.Data)
				}
				var total int64
				for _, dp := range c.DataPoints {
					total += dp.Value
				}
				if total != 1 {
					t.Fatalf("error count = %d, want 1", total)
				}
			}
		}
	}
	if !sawDuration || !sawErrors {
		t.Fatalf("missing instruments: duration=%v errors=%v", sawDuration, sawErrors)
	}
}

// TestNoProviderIsSafe checks the zero-configuration path: with no providers
// the global no-op ones are used and nothing panics.
func TestNoProviderIsSafe(t *testing.T) {
	s, err := graphql.NewSchema(graphql.SDL(sdl),
		graphql.Object[user]("User",
			graphql.Field("id", func(u *user) string { return u.ID }),
			graphql.Field("name", func(u *user) string { return u.Name }),
			graphql.Resolve("boom", func(context.Context, *user) (string, error) {
				return "", errors.New("boom")
			}),
		),
		graphql.Query(graphql.Resolve("me", func(context.Context, graphql.Root) (*user, error) {
			return &user{ID: "1"}, nil
		})),
		graphql.Subscription(
			graphql.Subscribe("ticks", func(context.Context) (<-chan int, error) { return nil, nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s, gqlotel.New()...)
	resp := e.Execute(context.Background(), &graphql.Request{Query: `{ me { id } }`})
	defer resp.Release()
	if got := string(resp.Data); !strings.Contains(got, `"id":"1"`) {
		t.Fatalf("data = %s", got)
	}
}

// TestParseFailureIsCountedOnce covers the layer split from the other side: a
// document that never parses never reaches the operation chain, so the request
// layer records it, and must still record it only once.
func TestParseFailureIsCountedOnce(t *testing.T) {
	h := newHarness(t)
	h.run(t, `{ me {`, "")

	var rm metricdata.ResourceMetrics
	if err := h.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var count uint64
	var errs int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != gqlotel.ScopeName {
			continue
		}
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					count += dp.Count
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					errs += dp.Value
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("duration recorded %d times, want 1", count)
	}
	if errs != 1 {
		t.Fatalf("error count = %d, want 1", errs)
	}
}
