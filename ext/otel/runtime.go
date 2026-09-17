package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
)

// AttrPlanCacheResult splits graphqlgo.plan_cache.lookups into hits and misses.
const AttrPlanCacheResult = attribute.Key("graphqlgo.plan_cache.result")

// ObserveExecutor reports e's shared state as asynchronous metrics, read from
// Executor.Stats once per collection: the plan cache's size, bytes and
// lookups (split by hit or miss), and the resolver concurrency slots in use
// and available. It is a separate call from New because the executor has to
// exist before anything can read it, the same shape as the OpenTelemetry
// runtime instrumentation. Unregister the returned registration when e is
// retired.
//
// Observe each executor once per meter. Observing it twice counts every
// lookup twice, and two executors on one meter need WithAttributes to stay
// separate series. Only WithMeterProvider and WithAttributes have any effect.
func ObserveExecutor(e *graphql.Executor, opts ...Option) (metric.Registration, error) {
	c := newConfig(opts)
	m := c.meter
	// Usage and limits are UpDownCounters rather than gauges, as the
	// OpenTelemetry runtime and connection-pool conventions have them, so a
	// backend may add them up across instances.
	count, err := m.Int64ObservableUpDownCounter("graphqlgo.plan_cache.count",
		metric.WithUnit("{document}"),
		metric.WithDescription("Parsed documents held by the plan cache."))
	if err != nil {
		return nil, err
	}
	bytes, err := m.Int64ObservableUpDownCounter("graphqlgo.plan_cache.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Query text held by the plan cache, the quantity WithPlanCacheBytes bounds."))
	if err != nil {
		return nil, err
	}
	lookups, err := m.Int64ObservableCounter("graphqlgo.plan_cache.lookups",
		metric.WithUnit("{operation}"),
		metric.WithDescription("Operations planned, by whether the compiled plan was cached (hit) or compiled (miss)."))
	if err != nil {
		return nil, err
	}
	inUse, err := m.Int64ObservableUpDownCounter("graphqlgo.executor.concurrency.in_use",
		metric.WithUnit("{slot}"),
		metric.WithDescription("Resolver concurrency slots held right now."))
	if err != nil {
		return nil, err
	}
	limit, err := m.Int64ObservableUpDownCounter("graphqlgo.executor.concurrency.limit",
		metric.WithUnit("{slot}"),
		metric.WithDescription("Resolver concurrency slots available, the WithMaxConcurrency bound."))
	if err != nil {
		return nil, err
	}
	base := metric.WithAttributes(c.attrs...)
	hit := metric.WithAttributes(withAttr(c.attrs, AttrPlanCacheResult.String("hit"))...)
	miss := metric.WithAttributes(withAttr(c.attrs, AttrPlanCacheResult.String("miss"))...)
	return m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := e.Stats()
		o.ObserveInt64(count, int64(s.PlanCacheEntries), base)
		o.ObserveInt64(bytes, s.PlanCacheBytes, base)
		o.ObserveInt64(lookups, s.PlanCacheHits, hit)
		o.ObserveInt64(lookups, s.PlanCacheMisses, miss)
		o.ObserveInt64(inUse, int64(s.ConcurrencyInUse), base)
		o.ObserveInt64(limit, int64(s.ConcurrencyLimit), base)
		return nil
	}, count, bytes, lookups, inUse, limit)
}

// ObserveDrain reports how many long-lived connections — WebSocket
// connections and SSE subscription streams on handlers built with WithDrain —
// are open right now. Handlers without a Drain are not counted, and neither
// are single-result SSE requests. Observe each drain once per meter, with
// WithAttributes to tell several apart. Only WithMeterProvider and
// WithAttributes have any effect.
func ObserveDrain(d *drain.Drain, opts ...Option) (metric.Registration, error) {
	c := newConfig(opts)
	active, err := c.meter.Int64ObservableUpDownCounter("graphqlgo.transport.active_connections",
		metric.WithUnit("{connection}"),
		metric.WithDescription("Long-lived WebSocket connections and SSE subscription streams open."))
	if err != nil {
		return nil, err
	}
	base := metric.WithAttributes(c.attrs...)
	return c.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(active, int64(d.Active()), base)
		return nil
	}, active)
}

// withAttr returns attrs plus one more, without writing into attrs' array.
func withAttr(attrs []attribute.KeyValue, kv attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs)+1)
	return append(append(out, attrs...), kv)
}
