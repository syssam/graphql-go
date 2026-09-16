package otel

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/syssam/graphql-go/loader"
)

// Loader attributes.
const (
	// AttrLoaderKeys is how many distinct keys the flush carried, which is
	// the number that says whether batching is working: a loader reporting
	// one key per span has degraded to N+1.
	AttrLoaderKeys = attribute.Key("graphqlgo.loader.keys")
	// AttrLoaderKeyErrors counts keys that failed individually. A batch with
	// some of these is not a failed batch.
	AttrLoaderKeyErrors = attribute.Key("graphqlgo.loader.key_errors")
)

// Batch wraps a loader batch function so every flush gets a span under the
// request that caused it.
//
// The instrumentation lives here rather than in the loader package so that
// package keeps depending on nothing but the engine and the standard
// library: a DataLoader user who wants no OpenTelemetry should not compile
// it in.
//
//	loader.New(otel.Batch("user", loadUsers))
func Batch[K comparable, V any](name string, fn loader.BatchFunc[K, V], opts ...Option) loader.BatchFunc[K, V] {
	tracer := batchTracer(opts)
	return func(ctx context.Context, keys []K) (map[K]V, error) {
		ctx, span := startBatch(ctx, tracer, name, len(keys))
		defer span.End()
		out, err := fn(ctx, keys)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return out, err
	}
}

// MappedBatch is Batch for a per-key batch function. Keys that failed on
// their own are counted on the span but do not fail it: the batch did its
// job, and the fields that asked for those keys report their own errors.
func MappedBatch[K comparable, V any](name string, fn loader.MappedBatchFunc[K, V], opts ...Option) loader.MappedBatchFunc[K, V] {
	tracer := batchTracer(opts)
	return func(ctx context.Context, keys []K) (map[K]V, map[K]error, error) {
		ctx, span := startBatch(ctx, tracer, name, len(keys))
		defer span.End()
		out, keyErrs, err := fn(ctx, keys)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		if len(keyErrs) > 0 {
			span.SetAttributes(AttrLoaderKeyErrors.Int(len(keyErrs)))
		}
		return out, keyErrs, err
	}
}

func batchTracer(opts []Option) trace.Tracer {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.tracer == nil {
		c.tracer = otel.GetTracerProvider().Tracer(ScopeName)
	}
	return c.tracer
}

func startBatch(ctx context.Context, tracer trace.Tracer, name string, keys int) (context.Context, trace.Span) {
	return tracer.Start(ctx, "loader "+name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(AttrLoaderKeys.Int(keys)))
}
