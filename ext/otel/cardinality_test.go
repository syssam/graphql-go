package otel_test

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/syssam/graphql-go"
	gqlotel "github.com/syssam/graphql-go/ext/otel"
)

// durationSeries collects the duration histogram and returns one entry per
// series, each the operation name it carries ("" when it carries none).
func durationSeries(t *testing.T, h *harness) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "graphql.server.request.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				v, _ := dp.Attributes.Value(gqlotel.AttrOperationName)
				names = append(names, v.AsString())
			}
		}
	}
	return names
}

// An operation's name is whatever the client wrote, so as a metric attribute
// it lets an anonymous client mint a series per request: the SDK's cap filled,
// every operation after it landed in the overflow series, and the strings
// stayed in memory. A request that never parsed was worse, since its
// operationName is not even an identifier -- 64 KiB of it per series.
func TestOperationNamesAreNotAMetricDimensionByDefault(t *testing.T) {
	h := newHarness(t)
	for i := range 50 {
		h.run(t, fmt.Sprintf(`query Op%d { me { id } }`, i), "")
		h.run(t, `{ not a query`, fmt.Sprintf("Raw%d", i))
	}
	series := durationSeries(t, h)
	if len(series) > 2 {
		t.Fatalf("100 requests made %d duration series, want one for queries and one for requests that never parsed", len(series))
	}
	for _, name := range series {
		if name != "" {
			t.Fatalf("a series carries the operation name %q", name)
		}
	}
}

// Opted in, the name of a parsed operation is a dimension. What a request
// merely claimed as its operationName never is.
func TestOperationNameMetricsAreOptIn(t *testing.T) {
	h := newHarness(t, gqlotel.WithOperationNameMetrics(true))
	h.run(t, `query Checkout { me { id } }`, "")
	h.run(t, `{ not a query`, "Claimed")

	var sawCheckout bool
	for _, name := range durationSeries(t, h) {
		switch name {
		case "Checkout":
			sawCheckout = true
		case "Claimed":
			t.Fatal("the operationName of a request that never parsed became a series")
		}
	}
	if !sawCheckout {
		t.Fatal("the parsed operation's name is not on its series")
	}
}

// "This is a subscription event" was decided by there being no recording span
// in the context. Under any instrumented caller -- otelhttp around an SSE
// handler -- there is one, so events got no span of their own, their errors
// were recorded nowhere, and the caller's span was renamed after the
// subscription.
func TestSubscriptionEventsGetTheirOwnSpansUnderACallersSpan(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, parent := sdktrace.NewTracerProvider().Tracer("caller").Start(ctx, "POST /graphql/stream")

	events, err := h.exec.Subscribe(ctx, &graphql.Request{Query: `subscription Feed { ticks }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		h.ticks <- 1
		h.ticks <- 2
		close(h.ticks)
	}()
	for resp := range events {
		resp.Release()
	}

	spans := h.spans.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d event spans, want one per event", len(spans))
	}
	for _, s := range spans {
		if s.Name() != "subscription Feed" {
			t.Fatalf("event span name = %q", s.Name())
		}
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Fatal("an event span is not parented to the span that was active at Subscribe")
		}
	}
	if got := parent.(sdktrace.ReadOnlySpan).Name(); got != "POST /graphql/stream" {
		t.Fatalf("the caller's span was renamed to %q", got)
	}
}
