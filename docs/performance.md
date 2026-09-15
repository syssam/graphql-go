# Performance and conformance

Where the evidence lives, and what it says. Every figure here is measured;
none is estimated. Detail, method and caveats are in the linked documents.

| Dimension | Result | Detail |
|---|---|---|
| Query execution | **3.6-89x** faster in process | [compare](../compare/README.md) |
| Over HTTP, saturated | **4.5-5.6x** faster | [compare](../compare/README.md) |
| Allocations per request | **9-61x** fewer | [compare](../compare/README.md) |
| Code generation | **95x** faster, **56x** less memory | [benchmarks](benchmarks.md) |
| Compile time | roughly level | [benchmarks](benchmarks.md) |
| Edit to rebuilt | **3.4x** faster | [benchmarks](benchmarks.md) |
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

Generation memory was 105x and is now 56x: `codegen` links
`golang.org/x/tools/go/packages` for auto-bind, which every user of `gqlc`
pays for whether or not they enable it.

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

## List results

`iter.Seq[E]` list fields were added so a resolver can hand the executor
elements one at a time instead of building a `[]E` first. Measured whether
that shows up as fewer allocations per request: `BenchmarkListResultsSlice`
(`{users{id name}}`) against `BenchmarkListResultsSeq` (`{usersSeq{id name}}`),
same fixture, same field selection, `benchstat`, `n=10`, both benchmarks run
in one process so machine state is shared:

| | slice (`users`) | seq (`usersSeq`) | delta |
|---|---:|---:|---:|
| B/op | 1.064Ki | 1.188Ki | +11.65% (p=0.000) |
| allocs/op | 20.00 | 23.00 | +15.00% (p=0.000) |

**The seq path does not allocate less here — it allocates measurably more**,
both by bytes and by count, with p=0.000 across 10 runs each side. This is the
opposite of the feature's original allocation claim, and the honest result to
report rather than the hoped-for one.

The extra 3 allocations match `usersSeq`'s shape: it still builds an `ids`
slice up front and closes over it in the yield func, so the executor pays for
a slice *and* a closure plus the range-over-func state machine the compiler
generates to drive `iter.Seq`, where the plain `users` resolver pays for the
slice alone. `iter.Seq` only wins the allocation argument for a resolver that
would otherwise have to materialize a full `[]E` before it can start
returning results — a paginated store read, a database cursor, a
generator — not for a resolver, like this fixture's, that already holds a
slice and merely wraps it in a yield loop. The feature's value there is API
shape (streaming without a slice type), not fewer allocations.

Reproduce (benchstat compares same-named benchmarks across files, so split the
combined output in two, renaming both to a shared name):

```sh
go test -run '^$' -bench 'BenchmarkListResultsSlice|BenchmarkListResultsSeq' -benchmem -count=10 . > raw.txt
grep -E '^(goos|goarch|pkg|cpu|BenchmarkListResultsSlice)' raw.txt | sed 's/Slice//' > slice.txt
grep -E '^(goos|goarch|pkg|cpu|BenchmarkListResultsSeq)' raw.txt | sed 's/Seq//' > seq.txt
benchstat slice.txt seq.txt
```

### A resolver that never materializes

`usersSeq` above wraps a slice it already built, which measures wrapping
overhead, not the design doc's actual claim — a resolver that "stops building
a slice it only ever hands to the writer once", motivated by a database
cursor or paginated API that would otherwise force a `[]E` into existence
just to satisfy the return type. `BenchmarkLazySeqSlice` and
`BenchmarkLazySeqLazy` isolate that claim: both generate the same 1000
elements with the same per-element work (`newLazyItem`, an allocation plus an
`Itoa`), against a `LazyItem` type with only pure `Field` bindings so the list
takes `writeList`'s sequential path — the only path where streaming actually
happens, since the concurrent path drains a seq into a `[]any` up front by
design. `BenchmarkLazySeqSlice` builds a `[]*lazyItem` of all 1000 before
returning it; `BenchmarkLazySeqLazy` yields each element as it is generated
and never holds a backing array. 1000 elements is large enough that a
1000-pointer backing array (8 bytes each, ~8 KiB) is not lost in the noise of
schema lookup, plan-cache hit and JSON encoding that every iteration also
pays for. `TestLazySeqBenchmarkUsesSequentialPath` in `bench_lazyseq_test.go`
checks the sequential-path assumption directly, by failing a non-null element
partway through and confirming the generator stops within a couple of
elements rather than running to completion — the signature of the concurrent
path's eager drain.

`benchstat`, `n=10`, both benchmarks run in one process:

| | slice (materializes) | lazy (never materializes) | delta |
|---|---:|---:|---:|
| B/op | 94.12Ki | 86.20Ki | -8.41% (p=0.000) |
| allocs/op | 3.914k | 3.915k | +0.03% (p=0.000) |
| sec/op | 181.0µ ± 17% | 195.6µ ± 4% | ~ (p=0.063, not significant) |

**Here the saving is real, and it is exactly the backing array**: dropping a
1000-element `[]*lazyItem` saves ~7.9 KiB, which is what 1000 eight-byte
pointers plus a slice header costs, and nothing else changes since both sides
build the same 1000 `*lazyItem` values. But it shows up only in bytes, not in
allocation count: the lazy side spends the array's one allocation on the
`iter.Seq` closure and range-over-func state instead, netting +1 alloc
(3915 vs 3914) — a wash on the count that this project's CLAUDE.md says to
trust over timing. The timing delta is not significant (p=0.063) and should
not be read as a conclusion either way.

So both measurements are true at once, for different resolver shapes: a
resolver that already holds a slice pays more to wrap it in `iter.Seq` (the
first benchmark), and a resolver that would otherwise have to build a slice
purely to satisfy the return type saves that slice's bytes, though not a
whole allocation, by not building it (this one). `iter.Seq` is worth reaching
for when the source is genuinely incremental — a cursor, a paginated fetch, a
generator that cannot produce a length up front — not as a reflexive
replacement for a resolver that already has a `[]E` in hand.

Reproduce:

```sh
go test -run '^$' -bench 'BenchmarkLazySeqSlice|BenchmarkLazySeqLazy' -benchmem -count=10 . > raw.txt
grep -E '^(goos|goarch|pkg|cpu|BenchmarkLazySeqSlice)' raw.txt | sed 's/LazySeqSlice/LazySeq/' > slice.txt
grep -E '^(goos|goarch|pkg|cpu|BenchmarkLazySeqLazy)' raw.txt | sed 's/LazySeqLazy/LazySeq/' > lazy.txt
benchstat slice.txt lazy.txt
```

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

## Profile-guided optimization

PGO cannot be shipped with this library. `go build` selects `default.pgo` from
the directory of each *main package* and applies it to that binary's
dependencies; a profile checked in here would be read only if someone built a
main package inside this repository. The profile has to be collected from the
consumer's own server and live beside their `main`.

Measured on this machine, runs interleaved, `n=10`, profile collected from the
root benchmarks themselves:

| Benchmark | vs `-pgo=off` |
|---|---|
| `ExecuteUsers` | -11.96% (p=0.027) |
| `ExecuteConcurrentList` | no change (p=0.853) |

Allocation counts and bytes do not move at all: PGO changes inlining and
devirtualization, not what gets allocated.

The split is the useful part. `ExecuteUsers` is one goroutine walking a plan
through indirect closure calls, which is exactly what PGO devirtualizes.
`ExecuteConcurrentList` spends its time in `runtime.lock2`, `semasleep` and
`semawakeup` under the bounded semaphore — scheduler contention that PGO cannot
reach. Expect a gain on CPU-bound, resolver-light queries and nothing on queries
dominated by concurrent fan-out.

Treat -11.96% as an upper bound. The profile was collected from the same
benchmarks it was then measured against, which flatters it; a profile taken from
a real workload predicts that workload, not this one. The confidence intervals
are also wide (+/-13% and +/-23%), which is the usual warning about this machine.

To enable it, from the server's own main package:

```sh
curl -o cpu.prof 'http://localhost:6060/debug/pprof/profile?seconds=30'
mv cpu.prof default.pgo   # beside main.go
go build                  # -pgo=auto is already the default
```

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
