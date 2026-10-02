// Package otel instruments an Executor with OpenTelemetry traces and metrics.
//
// Install it as executor options:
//
//	e := graphql.NewExecutor(schema, otel.New()...)
//
// One span covers a request. It starts before parsing, so a document that
// fails to parse still produces a span, and is renamed once the operation is
// known — a span called "query UserPage" is worth more than one called
// "graphql.request", but the name is only available after parsing.
//
// Subscriptions are the exception to "one span per request": Executor.
// Subscribe does not run the request chain, and the operation chain runs once
// per event. Each event therefore gets its own operation span, parented to
// whatever span was active when Subscribe was called, which is the useful
// shape — an event is the unit of work, and a subscription open for a day
// should not be one span.
package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/syssam/graphql-go"
)

// ScopeName identifies this instrumentation to a tracer or meter provider.
const ScopeName = "github.com/syssam/graphql-go/ext/otel"

// Attribute keys, following the OpenTelemetry GraphQL semantic conventions.
const (
	AttrOperationName = attribute.Key("graphql.operation.name")
	AttrOperationType = attribute.Key("graphql.operation.type")
	AttrDocument      = attribute.Key("graphql.document")

	// Attributes with no convention, kept under a distinct prefix so they are
	// obviously ours rather than mistaken for standard ones.
	AttrFieldPath = attribute.Key("graphqlgo.field.path")
	AttrCacheHit  = attribute.Key("graphqlgo.plan.cache_hit")
	// AttrPlanUncacheable is set only when true: a cache_hit that is false
	// forever means something different from one that is false because the
	// document is new, and only this tells them apart.
	AttrPlanUncacheable = attribute.Key("graphqlgo.plan.uncacheable")
	AttrComplexity      = attribute.Key("graphqlgo.operation.complexity")
	AttrDepth           = attribute.Key("graphqlgo.operation.depth")
	AttrErrorCount      = attribute.Key("graphqlgo.response.error_count")
	AttrFieldObject     = attribute.Key("graphqlgo.field.object")
)

type config struct {
	tracer     trace.Tracer
	meter      metric.Meter
	fieldSpans bool
	// resolverSpans spans only the fields FieldInfo.Resolver marks.
	resolverSpans bool
	recordQuery   bool
	// nameMetrics puts a parsed operation's name on the request metrics.
	nameMetrics bool
	duration    metric.Float64Histogram
	errors      metric.Int64Counter
	active      metric.Int64UpDownCounter
	attrs       []attribute.KeyValue
}

// Option configures the instrumentation.
type Option func(*config)

// WithTracerProvider sets the provider spans are created from. The default is
// otel.GetTracerProvider().
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracer = tp.Tracer(ScopeName) }
}

// WithMeterProvider sets the provider metrics are created from. The default is
// otel.GetMeterProvider().
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meter = mp.Meter(ScopeName) }
}

// WithAttributes adds attributes to every observation ObserveExecutor and
// ObserveDrain make. It is what keeps two executors or drains on one meter
// apart: their instruments share names, so without distinct attributes all
// their values add into one series, and observing one executor twice doubles
// every value.
// Name each one, say graphqlgo.executor.name. It does not affect New.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) { c.attrs = append(c.attrs, attrs...) }
}

// WithFieldSpans emits a span per field. It is off by default. It is wired
// through a FieldObserver rather than a FieldInterceptor: an observer cannot
// change a field's result, so pure fields keep their typed write path instead
// of being routed through the type-erased one, which is what the interceptor
// form used to cost.
func WithFieldSpans(enabled bool) Option {
	return func(c *config) { c.fieldSpans = enabled }
}

// WithResolverSpans emits a span per resolver field -- bound with Resolve or
// ResolveArgs -- and none for pure fields. A resolver is where a field does
// I/O, so a database or RPC client instrumented at its own layer (otelsql,
// otelgrpc) lands its spans under the resolver that issued them, and a
// page of fifty rows is not also fifty spans per column. WithFieldSpans,
// which spans every field, includes these.
func WithResolverSpans(enabled bool) Option {
	return func(c *config) { c.resolverSpans = enabled }
}

// WithDocument records the query text on the span. It is off by default
// because a document can carry data in inline arguments, and traces are
// usually retained longer and read more widely than logs.
func WithDocument(enabled bool) Option {
	return func(c *config) { c.recordQuery = enabled }
}

// WithOperationNameMetrics adds graphql.operation.name to the request duration
// and error metrics. It is off by default because the name is whatever the
// client wrote: each distinct one is a new series, so an anonymous client can
// fill the SDK's series limit, after which every operation it has not seen
// yet lands in the overflow series. Turn it on where clients cannot choose
// names -- behind a safelist such as ext/trusted. Spans carry the name either
// way. The operationName of a request that never parsed is not a name at all
// and is never a metric attribute.
func WithOperationNameMetrics(enabled bool) Option {
	return func(c *config) { c.nameMetrics = enabled }
}

// New returns the executor options that install the instrumentation.
func New(opts ...Option) []graphql.ExecutorOption {
	c := newConfig(opts)
	// Instrument creation fails only on a bad name, which is a constant here;
	// a nil instrument is simply not recorded, so a failure degrades to traces
	// only rather than taking the server down.
	c.duration, _ = c.meter.Float64Histogram("graphql.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a GraphQL operation."))
	c.errors, _ = c.meter.Int64Counter("graphql.server.errors",
		metric.WithDescription("GraphQL errors returned to clients."))
	c.active, _ = c.meter.Int64UpDownCounter("graphql.server.active_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("GraphQL queries and mutations being served right now. Subscriptions are not counted; see graphqlgo.transport.active_connections."))

	out := []graphql.ExecutorOption{
		graphql.WithRequestInterceptor(graphql.RequestInterceptorFunc(c.interceptRequest)),
		graphql.WithOperationInterceptor(graphql.OperationInterceptorFunc(c.interceptOperation)),
	}
	if c.fieldSpans || c.resolverSpans {
		out = append(out, graphql.WithFieldObserver(fieldSpanObserver{c}))
	}
	return out
}

// newConfig applies opts and fills in the global providers for anything unset.
func newConfig(opts []Option) *config {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.tracer == nil {
		c.tracer = otel.GetTracerProvider().Tracer(ScopeName)
	}
	if c.meter == nil {
		c.meter = otel.GetMeterProvider().Meter(ScopeName)
	}
	return c
}

// interceptRequest opens the span that covers everything, including parsing.
func (c *config) interceptRequest(ctx context.Context, req *graphql.Request, next graphql.RequestHandler) *graphql.Response {
	// Counted around the whole chain, parsing included, as HTTP's
	// active_requests counts around the whole handler. A nil instrument
	// (creation failed) is skipped rather than recorded.
	if c.active != nil {
		c.active.Add(ctx, 1)
		defer c.active.Add(context.WithoutCancel(ctx), -1)
	}
	ctx, span := c.tracer.Start(ctx, "graphql.request", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	if req.OperationName != "" {
		span.SetAttributes(AttrOperationName.String(req.OperationName))
	}
	if c.recordQuery {
		span.SetAttributes(AttrDocument.String(req.Query))
	}

	// The operation layer owns the metrics, because that is where the
	// operation type is known and it is the better dimension to slice by.
	// This flag lets the request layer record only when the operation chain
	// never ran, so a parse failure is counted exactly once and a successful
	// request is not counted twice.
	recorded := new(bool)
	ctx = context.WithValue(ctx, recordedKey{}, recorded)

	start := time.Now()
	resp := next(ctx, req)
	c.recordSpan(span, resp)
	if !*recorded {
		// No attributes: the operation never ran, so its type is unknown, and
		// the operationName beside the document is unvalidated client text.
		c.recordMetrics(ctx, resp, start, nil)
	}
	return resp
}

// recordedKey carries the flag that keeps a metric from being recorded at
// both layers of the chain.
type recordedKey struct{}

// interceptOperation names the span now that the operation is known and adds
// what only the compiled plan can say.
func (c *config) interceptOperation(ctx context.Context, oc *graphql.OperationContext, next graphql.OperationHandler) *graphql.Response {
	span := trace.SpanFromContext(ctx)
	kind := string(oc.Operation.Operation)
	name := oc.Operation.Name

	// A subscription event never passed through interceptRequest, so it has no
	// span of ours to rename and needs one of its own. The flag that layer
	// leaves in the context is what says so. Whether a span is recording does
	// not: under an instrumented caller one always is, and it is the caller's.
	flag, viaRequest := ctx.Value(recordedKey{}).(*bool)
	own := !viaRequest
	if own {
		var sub trace.Span
		ctx, sub = c.tracer.Start(ctx, spanName(kind, name), trace.WithSpanKind(trace.SpanKindServer))
		defer sub.End()
		span = sub
	} else {
		span.SetName(spanName(kind, name))
	}

	attrs := []attribute.KeyValue{
		AttrOperationType.String(kind),
		AttrCacheHit.Bool(oc.Stats.CacheHit),
		AttrComplexity.Int(oc.Complexity()),
		AttrDepth.Int(oc.Depth()),
	}
	if name != "" {
		attrs = append(attrs, AttrOperationName.String(name))
	}
	if oc.Stats.PlanUncacheable {
		attrs = append(attrs, AttrPlanUncacheable.Bool(true))
	}
	span.SetAttributes(attrs...)

	start := time.Now()
	resp := next(ctx, oc)

	metricAttrs := []attribute.KeyValue{AttrOperationType.String(kind)}
	if name != "" && c.nameMetrics {
		metricAttrs = append(metricAttrs, AttrOperationName.String(name))
	}
	c.recordMetrics(ctx, resp, start, metricAttrs)
	if viaRequest {
		*flag = true
	}
	// A subscription event owns its span, so it also applies the outcome; on
	// a request the enclosing layer does it once, over the whole request.
	if own {
		c.recordSpan(span, resp)
	}
	return resp
}

// fieldSpanObserver emits a span per field. It keeps no state: Start returns a
// context carrying the span and EndField is handed that same context back, so
// there is nothing to store between the two.
type fieldSpanObserver struct{ c *config }

func (o fieldSpanObserver) spans(f graphql.FieldInfo) bool {
	return o.c.fieldSpans || f.Resolver
}

func (o fieldSpanObserver) BeginField(ctx context.Context, f graphql.FieldInfo) context.Context {
	if !o.spans(f) {
		return ctx
	}
	ctx, span := o.c.tracer.Start(ctx, f.Object+"."+f.Field)
	span.SetAttributes(
		AttrFieldObject.String(f.Object),
		AttrFieldPath.String(f.Path().String()),
	)
	return ctx
}

func (o fieldSpanObserver) EndField(ctx context.Context, f graphql.FieldInfo, err error) {
	if !o.spans(f) {
		// BeginField started nothing: the span in ctx is the parent's.
		return
	}
	span := trace.SpanFromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// recordSpan applies the outcome of a response to a span. Field errors mark
// it as an error too: a 200 carrying errors is still a request that did not do
// what was asked.
func (c *config) recordSpan(span trace.Span, resp *graphql.Response) {
	n := errorCount(resp)
	span.SetAttributes(AttrErrorCount.Int(n))
	if n == 0 {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetStatus(codes.Error, resp.Errors[0].Message)
	for _, e := range resp.Errors {
		span.RecordError(e)
	}
}

func (c *config) recordMetrics(ctx context.Context, resp *graphql.Response, start time.Time, attrs []attribute.KeyValue) {
	set := metric.WithAttributes(attrs...)
	if c.duration != nil {
		c.duration.Record(ctx, time.Since(start).Seconds(), set)
	}
	if n := errorCount(resp); c.errors != nil && n > 0 {
		c.errors.Add(ctx, int64(n), set)
	}
}

func errorCount(resp *graphql.Response) int {
	if resp == nil {
		return 0
	}
	return len(resp.Errors)
}

// spanName follows the convention of "<type> <name>", falling back to the
// type alone for an anonymous operation rather than leaving a trailing space.
func spanName(kind, name string) string {
	if name == "" {
		return kind
	}
	return kind + " " + name
}
