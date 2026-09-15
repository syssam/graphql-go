# Performance and conformance

Where the evidence lives, and what it says. Every figure here is measured;
none is estimated. Detail, method and caveats are in the linked documents.

| Dimension | Result | Detail |
|---|---|---|
| Query execution | **3.6-89x** faster in process | [compare](../compare/README.md) |
| Over HTTP, saturated | **4.5-5.6x** faster | [compare](../compare/README.md) |
| Allocations per request | **9-61x** fewer | [compare](../compare/README.md) |
| Code generation | **95x** faster, **105x** less memory | [benchmarks](benchmarks.md) |
| Compile time | roughly level | [benchmarks](benchmarks.md) |
| Edit to rebuilt | **3.2x** faster | [benchmarks](benchmarks.md) |
| **Schema build** | **2 180x slower** | [compare](../compare/README.md) |
| **Memory retained** | **278x more** | [compare](../compare/README.md) |
| Subscription broadcast | **36 allocs** per subscriber, flat 1→128 | [gqlws](../transport/gqlws/load_test.go) |
| GraphQL over HTTP spec | **0 errors**, 13/13 MUST | [audit](graphql-http-audit.md) |

Measured against gqlgen 0.17.95 on a 200-entity ORM-shaped schema (~1 600
types), Go 1.27.1, Windows, i7-12700.

## The shape of it

**What graphql-go buys.** A request costs what the query costs, not what the
schema costs. gqlgen allocates 975 objects to answer a two-field query against
a 200-entity schema and 168 against a 2-entity one; graphql-go allocates 16
either way. That is the compiled-plan design: the plan is built once per
operation, so schema size stops mattering at request time. Under saturation
this is 5.6x the throughput.

**What it costs.** Binding and validating the whole type graph at `NewSchema`
takes 30 ms and retains 11 MB, where gqlgen did that work at code generation
time and retains 0.04 MB. A long-lived server repays the build within a few
hundred requests; a short-lived process does not, and a memory-bound
deployment fits fewer replicas per node.

**What is a wash.** Compile time. graphql-go emits nine times less code but it
compiles no faster, because generic instantiation costs roughly eight times
more per line. The build-time win is in *generation* — 95x faster, 105x less
memory — and in incremental rebuilds, not in the compiler.

## Subscriptions

One broadcast reaching every subscriber, over a real WebSocket, counting
receipts rather than timing the publish:

| Subscribers | Per broadcast | Per subscriber | Allocations per subscriber |
|---:|---:|---:|---:|
| 1 | 26.8 us | 26.8 us | 36 |
| 16 | 124 us | 7.8 us | 36 |
| 128 | 518 us | 4.0 us | 36 |

Allocations per subscriber are the figure to read: flat from one client to a
hundred and twenty-eight, and unlike the timings they do not move with machine
noise. Per-subscriber time falls with scale because the round trip amortises.

Timing the publish instead of the delivery reports 45ns per subscriber at 128
clients, which is below the cost of encoding one response and should be
disbelieved on sight: `publish` drops into a full buffer rather than blocking,
so most of those events reached nobody.

150 concurrent subscriptions opened and closed return every source
registration and every goroutine. Both the pending and the idle case are
tested, and only the idle one is decisive — writes use the connection context,
so a subscription with an event pending is reclaimed by that write failing
whether or not cancellation works at all.

## Reproducing

```sh
cd compare && go run gen.go -n 200        # generate both engines
go test -run TestEnginesAgree .           # they must agree before timing them
go test -count=5 -run '^$' -bench . -benchmem .

cd benchmarks && go run ./cmd/buildbench -n 200 -split

go test -run '^$' -bench BenchmarkSubscriptionFanout -benchmem ./transport/gqlws
go test -run TestIdleSubscriptionsAreReleased ./transport/gqlws
```

Compare two versions with `benchstat`, never by eye: single samples on this
codebase have been wrong by 20-77% on a warm machine, in both directions.
Allocation counts and retained heap are deterministic and are the figures to
trust when timings are noisy.

## What is not measured

- **Latency percentiles.** This machine's monotonic clock has ~522 us
  granularity, so a 70 us request cannot be timed individually and the faster
  engine accumulates more unmeasurable samples. Needs a finer-clock platform;
  no load tool escapes this.
- **Behaviour under a cgroup memory limit** with `GOMEMLIMIT`, which is how a
  container actually runs. Linux only.
- **Subscriptions against another engine.** `BenchmarkSubscriptionFanout`
  measures this engine broadcasting over WebSocket, but nothing compares it
  with gqlgen: that needs gqlgen subscription resolvers generated into
  `compare/`, which does not exist yet. The comparison figures above are all
  request/response.
- **Anything above 150 concurrent connections.** The leak tests open 150,
  which is enough to find accumulation and small enough to stay clear of
  Windows ephemeral-port exhaustion. Whether behaviour holds at ten thousand
  is untested.
