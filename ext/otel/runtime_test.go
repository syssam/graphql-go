package otel_test

import (
	"context"
	"sync"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/syssam/graphql-go"
	gqlotel "github.com/syssam/graphql-go/ext/otel"
	"github.com/syssam/graphql-go/transport/drain"
)

// collectInts gathers every int64 gauge or sum in this package's scope into
// name -> summed value, so a test reads a metric the way a backend would.
func collectInts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != gqlotel.ScopeName {
			continue
		}
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					out[m.Name] += dp.Value
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					out[m.Name] += dp.Value
				}
			}
		}
	}
	return out
}

func TestObserveExecutorReportsStats(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	e := graphql.NewExecutor(newSchema(t, make(chan int)), graphql.WithMaxConcurrency(7))

	reg, err := gqlotel.ObserveExecutor(e, gqlotel.WithMeterProvider(mp))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.Execute(context.Background(), &graphql.Request{Query: `{ me { id } }`}).Release()
	}

	got := collectInts(t, reader)
	want := map[string]int64{
		"graphqlgo.plan_cache.entries":         1,
		"graphqlgo.plan_cache.bytes":           int64(len(`{ me { id } }`)),
		"graphqlgo.plan_cache.hits":            2,
		"graphqlgo.plan_cache.misses":          1,
		"graphqlgo.executor.concurrency.limit": 7,
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %d, want %d (all: %v)", name, got[name], v, got)
		}
	}
	if _, ok := got["graphqlgo.executor.concurrency.in_use"]; !ok {
		t.Errorf("graphqlgo.executor.concurrency.in_use not reported (all: %v)", got)
	}

	if err := reg.Unregister(); err != nil {
		t.Fatal(err)
	}
	if after := collectInts(t, reader); after["graphqlgo.plan_cache.entries"] != 0 {
		t.Errorf("still reporting after Unregister: %v", after)
	}
}

func TestObserveDrainReportsActiveConnections(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	d := drain.New()
	if _, err := gqlotel.ObserveDrain(d, gqlotel.WithMeterProvider(mp)); err != nil {
		t.Fatal(err)
	}
	_, leaveA, _ := d.Enter(context.Background())
	_, leaveB, _ := d.Enter(context.Background())
	defer leaveB()
	if got := collectInts(t, reader)["graphqlgo.transport.active_connections"]; got != 2 {
		t.Fatalf("active_connections = %d with two entered, want 2", got)
	}
	leaveA()
	if got := collectInts(t, reader)["graphqlgo.transport.active_connections"]; got != 1 {
		t.Fatalf("active_connections = %d after one left, want 1", got)
	}
}

// TestActiveRequestsTracksInFlight: the request interceptor counts a request
// while it runs and gives it back when it returns, including on errors.
func TestActiveRequestsTracksInFlight(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	entered := make(chan struct{})
	release := make(chan struct{})
	s, err := graphql.NewSchema(graphql.SDL(`type Query { wait: Int! bad: Int! }`),
		graphql.Query(
			graphql.Resolve("wait", func(context.Context, graphql.Root) (int, error) {
				close(entered)
				<-release
				return 1, nil
			}),
			graphql.Resolve("bad", func(context.Context, graphql.Root) (int, error) { return 0, context.Canceled }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s, gqlotel.New(gqlotel.WithMeterProvider(mp))...)

	var wg sync.WaitGroup
	wg.Go(func() { e.Execute(context.Background(), &graphql.Request{Query: `{ wait }`}).Release() })
	<-entered
	if got := collectInts(t, reader)["graphql.server.active_requests"]; got != 1 {
		t.Fatalf("active_requests = %d with one request running, want 1", got)
	}
	close(release)
	wg.Wait()
	e.Execute(context.Background(), &graphql.Request{Query: `{ bad }`}).Release()
	e.Execute(context.Background(), &graphql.Request{Query: `{ nope }`}).Release()
	if got := collectInts(t, reader)["graphql.server.active_requests"]; got != 0 {
		t.Fatalf("active_requests = %d with nothing running, want 0", got)
	}
}
