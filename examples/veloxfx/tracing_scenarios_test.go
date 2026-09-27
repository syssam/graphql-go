package veloxfx

import (
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
)

// On call, the question is which field made a page slow. With a
// TracerProvider in the graph every resolver is a span and every statement a
// span beneath the resolver that ran it -- including the eager loads field
// collection folded into that resolver's query -- and a pure field, which
// reads what was loaded, is not a span at all.
func TestScenarioTracesReachFromFieldToSQL(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	a := &sqlApp{}
	a.app = start(t, fx.Supply(fx.Annotate(tp, fx.As(new(trace.TracerProvider)))))
	a.storefront(6)

	before := len(sr.Ended())
	a.data(`{ customers { name orders(first: 2) { totalCount edges { node { id status } } } } }`)
	spans := sr.Ended()[before:]

	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	var customers sdktrace.ReadOnlySpan
	for _, s := range spans {
		byID[s.SpanContext().SpanID()] = s
		if s.Name() == "Query.customers" {
			customers = s
		}
		if s.Name() == "Customer.name" || s.Name() == "Order.status" {
			t.Errorf("a pure field got a span: %s", s.Name())
		}
	}
	if customers == nil {
		t.Fatalf("no Query.customers span among %d", len(spans))
	}
	isSQL := func(s sdktrace.ReadOnlySpan) bool {
		return strings.Contains(s.InstrumentationScope().Name, "otelsql")
	}
	// under reports whether s descends from a resolver span, and which.
	under := func(s sdktrace.ReadOnlySpan) string {
		for p, ok := byID[s.Parent().SpanID()]; ok; p, ok = byID[p.Parent().SpanID()] {
			if strings.Contains(p.Name(), ".") && !isSQL(p) {
				return p.Name()
			}
		}
		return ""
	}
	var sqlSpans int
	for _, s := range spans {
		if !isSQL(s) || s.SpanContext().TraceID() != customers.SpanContext().TraceID() {
			continue
		}
		sqlSpans++
		if r := under(s); r == "" {
			t.Errorf("SQL span %s hangs off the operation, not a resolver", s.Name())
		}
	}
	if sqlSpans == 0 {
		t.Fatal("the page ran no traced SQL; otelsql is not wired")
	}
	var fromCustomers int
	for _, s := range spans {
		if isSQL(s) && under(s) == "Query.customers" {
			fromCustomers++
		}
	}
	if fromCustomers == 0 {
		t.Error("no statement under Query.customers, which loads the customers and their orders")
	}
}
