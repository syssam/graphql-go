package otel

import (
	"context"

	"go.opentelemetry.io/otel/metric"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/drain"
)

// ObserveExecutor reports e's shared state as asynchronous metrics, read from
// Executor.Stats once per collection: the plan cache's entries, bytes, hits
// and misses, and the resolver concurrency slots in use and available. It is
// a separate call from New because the executor has to exist before anything
// can read it, the same shape as the OpenTelemetry runtime instrumentation.
// Unregister the returned registration when e is retired. Only
// WithMeterProvider among the options has any effect.
func ObserveExecutor(e *graphql.Executor, opts ...Option) (metric.Registration, error) {
	m := newConfig(opts).meter
	entries, err := m.Int64ObservableGauge("graphqlgo.plan_cache.entries",
		metric.WithUnit("{document}"),
		metric.WithDescription("Parsed documents held by the plan cache."))
	if err != nil {
		return nil, err
	}
	bytes, err := m.Int64ObservableGauge("graphqlgo.plan_cache.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Query text held by the plan cache, the quantity WithPlanCacheBytes bounds."))
	if err != nil {
		return nil, err
	}
	hits, err := m.Int64ObservableCounter("graphqlgo.plan_cache.hits",
		metric.WithUnit("{operation}"),
		metric.WithDescription("Operations whose compiled plan was already cached."))
	if err != nil {
		return nil, err
	}
	misses, err := m.Int64ObservableCounter("graphqlgo.plan_cache.misses",
		metric.WithUnit("{operation}"),
		metric.WithDescription("Operations that had to be compiled."))
	if err != nil {
		return nil, err
	}
	inUse, err := m.Int64ObservableGauge("graphqlgo.executor.concurrency.in_use",
		metric.WithUnit("{slot}"),
		metric.WithDescription("Resolver concurrency slots held right now."))
	if err != nil {
		return nil, err
	}
	limit, err := m.Int64ObservableGauge("graphqlgo.executor.concurrency.limit",
		metric.WithUnit("{slot}"),
		metric.WithDescription("Resolver concurrency slots available, the WithMaxConcurrency bound."))
	if err != nil {
		return nil, err
	}
	return m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := e.Stats()
		o.ObserveInt64(entries, int64(s.PlanCacheEntries))
		o.ObserveInt64(bytes, s.PlanCacheBytes)
		o.ObserveInt64(hits, s.PlanCacheHits)
		o.ObserveInt64(misses, s.PlanCacheMisses)
		o.ObserveInt64(inUse, int64(s.ConcurrencyInUse))
		o.ObserveInt64(limit, int64(s.ConcurrencyLimit))
		return nil
	}, entries, bytes, hits, misses, inUse, limit)
}

// ObserveDrain reports how many long-lived connections — WebSocket
// connections and SSE subscription streams on handlers built with WithDrain —
// are open right now. Handlers without a Drain are not counted. Only
// WithMeterProvider among the options has any effect.
func ObserveDrain(d *drain.Drain, opts ...Option) (metric.Registration, error) {
	m := newConfig(opts).meter
	active, err := m.Int64ObservableGauge("graphqlgo.transport.active_connections",
		metric.WithUnit("{connection}"),
		metric.WithDescription("Long-lived WebSocket connections and SSE subscription streams open."))
	if err != nil {
		return nil, err
	}
	return m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(active, int64(d.Active()))
		return nil
	}, active)
}
