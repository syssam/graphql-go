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
//
// # What is observed
//
// A query or mutation: one span and one duration record, whether it ran,
// failed to parse, was refused, or panicked outside a resolver.
//
// A subscription event: the same, per event.
//
// A subscription that fails to open: a span with error status and a record on
// both metrics, when the failure is one a SubscriptionInterceptor can see —
// the source refusing to open, the Authorizer denying it, or an interceptor
// registered after this one refusing it. An open that succeeds adds nothing:
// its events are the spans.
//
// Not observed: a subscription refused before its plan exists — a document
// that does not parse or validate, variables that do not coerce, a depth,
// complexity or cost limit. Executor.Subscribe answers those before any
// interceptor runs, so nothing installed through executor options can see
// them; count them at the transport.
//
// # What is recorded
//
// Neither the document nor any variable is recorded unless WithDocument asks.
// That covers error messages too: the engine composes the message of a parse,
// validation, input or operation-selection error from the client's text —
// "got invalid value" quotes the value — so by default such an error reaches
// the span as its extensions.code and nothing else. Every other message is
// the server's own and is recorded.
//
// An operation's name is the client's choice of text and length, so it is cut
// to 256 bytes on the span and in the span's name.
package otel

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

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

// WithDocument records the query text on the span, and with it the messages
// of errors that quote the document or a variable's value. It is off by
// default because a document can carry data in inline arguments, and traces
// are usually retained longer and read more widely than logs.
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
		graphql.WithSubscriptionInterceptor(graphql.SubscriptionInterceptorFunc(c.interceptSubscription)),
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

	// Unvalidated until the document parses: over GET it is whatever bytes the
	// query string held. The operation layer replaces it with the parsed name.
	if req.OperationName != "" {
		span.SetAttributes(AttrOperationName.String(boundName(req.OperationName)))
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
	// The executor recovers a panic above every interceptor, so one that
	// passes through here never returns from next: without this the request
	// that failed worst would be the one with no record and a clean span.
	// Re-panicking leaves the recovery, and its answer to the client, as is.
	defer func() {
		if r := recover(); r != nil {
			recordPanic(span)
			if !*recorded {
				c.recordMetrics(ctx, 1, start, nil)
			}
			panic(r)
		}
	}()
	resp := next(ctx, req)
	c.recordSpan(span, resp)
	if !*recorded {
		// No attributes: the operation never ran, so its type is unknown, and
		// the operationName beside the document is unvalidated client text.
		c.recordMetrics(ctx, errorCount(resp), start, nil)
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
	name := boundName(oc.Operation.Name)

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

	metricAttrs := c.metricAttrs(kind, name)
	start := time.Now()
	// As in interceptRequest. The flag is set here too, so the request layer
	// above marks its span and leaves the metrics to this record.
	defer func() {
		if r := recover(); r != nil {
			c.recordMetrics(ctx, 1, start, metricAttrs)
			if viaRequest {
				*flag = true
			}
			if own {
				recordPanic(span)
			}
			panic(r)
		}
	}()
	resp := next(ctx, oc)

	c.recordMetrics(ctx, errorCount(resp), start, metricAttrs)
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

// interceptSubscription records a subscription that fails to open. Nothing
// else would: no event follows, so the operation layer never runs for it.
//
// A successful open is left out of the trace. A span for it would have to be
// in the context the events inherit, which would parent every event to a
// span that ended before the first one arrived.
func (c *config) interceptSubscription(ctx context.Context, oc *graphql.OperationContext, next graphql.SubscriptionHandler) (<-chan *graphql.Response, error) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			c.recordFailedOpen(ctx, oc, start, 1, recordPanic)
			panic(r)
		}
	}()
	events, err := next(ctx, oc)
	if err == nil {
		return events, nil
	}

	// A SubscribeError carries what the client is sent. Any other error is an
	// interceptor's own, and is one failure with that message.
	resp := &graphql.Response{Errors: []*graphql.Error{graphql.Errorf("%v", err)}}
	var se *graphql.SubscribeError
	if errors.As(err, &se) && errorCount(se.Response) > 0 {
		resp = se.Response
	}
	c.recordFailedOpen(ctx, oc, start, errorCount(resp), func(span trace.Span) { c.recordSpan(span, resp) })
	return events, err
}

// recordFailedOpen is the span and the metrics of an open already known to
// have failed. The span is backdated to when the open began, so its duration
// is the open's, and outcome puts on it what went wrong.
func (c *config) recordFailedOpen(ctx context.Context, oc *graphql.OperationContext, start time.Time, errs int, outcome func(trace.Span)) {
	kind, name := string(oc.Operation.Operation), boundName(oc.Operation.Name)
	attrs := []attribute.KeyValue{AttrOperationType.String(kind)}
	if name != "" {
		attrs = append(attrs, AttrOperationName.String(name))
	}
	_, span := c.tracer.Start(ctx, spanName(kind, name),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(start),
		trace.WithAttributes(attrs...))
	outcome(span)
	span.End()
	c.recordMetrics(ctx, errs, start, c.metricAttrs(kind, name))
}

// metricAttrs are the dimensions of the request metrics for one operation.
func (c *config) metricAttrs(kind, name string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{AttrOperationType.String(kind)}
	if name != "" && c.nameMetrics {
		attrs = append(attrs, AttrOperationName.String(name))
	}
	return attrs
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
		err = o.c.recordable(err)
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
	for i, e := range resp.Errors {
		err := c.recordable(e)
		if i == 0 {
			// The message alone, as before: Error() on a *graphql.Error
			// leads with the path, which the span does not need twice.
			message := e.Message
			var code codeOnly
			if errors.As(err, &code) {
				message = string(code)
			}
			span.SetStatus(codes.Error, message)
		}
		span.RecordError(err)
	}
}

// recordable returns err as it may be put on a span: itself, or its code
// alone when its message quotes the client and WithDocument has not asked
// for the client's text.
func (c *config) recordable(err error) error {
	var e *graphql.Error
	if c.recordQuery || !errors.As(err, &e) || !quotesClient(e) {
		return err
	}
	return codeOnly(errorCode(e))
}

// recordPanic marks a span whose request is unwinding. The executor answers a
// recovered panic with one error, so that is the count.
func recordPanic(span trace.Span) {
	span.SetAttributes(AttrErrorCount.Int(1))
	span.SetStatus(codes.Error, "panic")
}

// quotesClient reports whether e's message is about the request's own text:
// these are the codes the engine gives an error about the document, the
// variables or the operation name, each of which quotes what it is about,
// and BAD_USER_INPUT is what a resolver uses to say the same of its input.
func quotesClient(e *graphql.Error) bool {
	switch errorCode(e) {
	case graphql.CodeParseFailed, graphql.CodeValidationFailed, graphql.CodeBadUserInput, graphql.CodeOperationResolution:
		return true
	}
	return false
}

func errorCode(e *graphql.Error) string {
	code, _ := e.Extensions["code"].(string)
	return code
}

// codeOnly stands on a span for an error whose message may not be recorded.
type codeOnly string

func (c codeOnly) Error() string { return string(c) }

func (c *config) recordMetrics(ctx context.Context, errs int, start time.Time, attrs []attribute.KeyValue) {
	set := metric.WithAttributes(attrs...)
	if c.duration != nil {
		c.duration.Record(ctx, time.Since(start).Seconds(), set)
	}
	if c.errors != nil && errs > 0 {
		c.errors.Add(ctx, int64(errs), set)
	}
}

func errorCount(resp *graphql.Response) int {
	if resp == nil {
		return 0
	}
	return len(resp.Errors)
}

// maxNameLen bounds an operation name wherever it is recorded. The grammar
// puts no limit on a name and a request body allows a megabyte of one; a
// span's name is an index key and its attributes are stored with every span.
const maxNameLen = 256

// boundName cuts s to maxNameLen bytes of valid UTF-8. The validity matters
// for a name that never parsed, which is raw request bytes: OTLP is protobuf,
// and protobuf does not encode a string that is not UTF-8.
func boundName(s string) string {
	if len(s) <= maxNameLen && utf8.ValidString(s) {
		return s
	}
	s = strings.ToValidUTF8(s[:min(len(s), maxNameLen)], "\uFFFD")
	for len(s) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// spanName follows the convention of "<type> <name>", falling back to the
// type alone for an anonymous operation rather than leaving a trailing space.
func spanName(kind, name string) string {
	if name == "" {
		return kind
	}
	return kind + " " + name
}
