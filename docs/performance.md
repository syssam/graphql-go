# Performance and conformance

Where the evidence lives, and what it says. Every figure here is measured;
none is estimated. Detail, method and caveats are in the linked documents.

| Dimension | Result | Detail |
|---|---|---|
| Query execution | **4.3-83x** faster in process | [compare](../compare/README.md) |
| Over HTTP, saturated | **4.5-5.6x** faster | [compare](../compare/README.md) |
| Allocations per request | **9.0-81x** fewer in process | [compare](../compare/README.md) |
| Code generation | **95x** faster, **56x** less memory | [benchmarks](benchmarks.md) |
| Compile time | roughly level | [benchmarks](benchmarks.md) |
| Edit to rebuilt | **3.4x** faster | [benchmarks](benchmarks.md) |
| **Schema build** | **2 180x slower** | [compare](../compare/README.md) |
| **Memory retained** | **278x more** | [compare](../compare/README.md) |
| Subscription broadcast | **36 allocs** per subscriber, flat 1→128 | [gqlws](../transport/gqlws/load_test.go) |
| Under k6, p95 at equal load | **22x** lower | [k6](../compare/k6/README.md) |
| GraphQL over HTTP spec | **0 errors**, 13/13 MUST | [audit](graphql-http-audit.md) |

Measured against gqlgen 0.17.95 on a 200-entity ORM-shaped schema (~1 600
types), Go 1.27.1, Windows, i7-12700. The in-process rows were re-measured on
2026-09-21 with both engines in one run; the HTTP and saturation rows are from
the earlier sitting and predate that allocation work, so they understate the
gap by about four allocations a request.

## The shape of it

**What graphql-go buys.** A request costs what the query costs, not what the
schema costs. gqlgen allocates 975 objects to answer a two-field query against
a 200-entity schema and 168 against a 2-entity one; graphql-go allocates 12
against the 200-entity schema, and answered both schemas with the identical
count when the pair was measured (16 each, before the allocation work in
`32d994e..d542c6b` took it to 12). That is the compiled-plan design: the plan is built once per
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

That asymmetry is now a gate. `TestAllocationBaseline` runs the benchmarks
whose allocation count is a contract and fails if any of them allocates more
than [`alloc-baseline.txt`](alloc-baseline.txt) records -- one-sided, so an
improvement logs rather than fails, and pinning the exact number does not turn
every win into a red build. Adding one allocation per request to
`newResponseWriter` fails it on seven benchmarks at once, each reporting
"a regression of 1".

It runs on Linux in CI, without `-race`, because the detector changes
allocation counts and the test skips itself under it. **The baseline was
recorded on Windows**; if Linux disagrees the first CI run says so, and the
fix is to regenerate it there:

```sh
go test -run TestAllocationBaseline -update-allocs .
```

Timings are published from the same job as an artifact and are never asserted
on. A shared runner cannot produce a number this repository would act on.

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

- ~~Latency percentiles of an unsaturated request.~~ Measured on Linux; see
  [Latency percentiles](#latency-percentiles) below. They remain unmeasurable
  *on Windows*, and the tool says so rather than printing a number.
- ~~Behaviour under a cgroup memory limit.~~ Measured; see
  [operations.md](operations.md#in-a-container).
- **Subscriptions against another engine.** `BenchmarkSubscriptionFanout`
  measures this engine broadcasting over WebSocket, but nothing compares it
  with gqlgen: that needs gqlgen subscription resolvers generated into
  `compare/`, which does not exist yet. The comparison figures above are all
  request/response.
- **Anything above 150 concurrent connections.** The leak tests open 150,
  which is enough to find accumulation and small enough to stay clear of
  Windows ephemeral-port exhaustion. Whether behaviour holds at ten thousand
  is untested.

## Latency percentiles

Withheld for a long time because this development machine could not resolve
them. `benchmarks/cmd/latency` now measures the clock before it measures the
engine and prints what it found, so the result carries its own validity:

```
windows/amd64  GOMAXPROCS=20  clock tick: 211.5us
shape            min        p50        p90        p99      p99.9        max
tiny              0s         0s         0s         0s    1.621ms  10.2783ms
WARNING: the clock resolves p50 of "tiny" to only 0.0 ticks; these percentiles
are the timer, not the engine.
```

Every p50 is exactly `0s`. That is the whole reason no percentile was ever
published here.

The same binary in `golang:1.27` on this machine's Docker (linux/amd64, 20
cores), 200 000 samples per shape:

```
linux/amd64  GOMAXPROCS=20  clock tick: 17ns
shape            min        p50        p90        p99      p99.9        max   ticks/p50
tiny         5.299us    5.892us    7.648us   36.256us   133.18us  2.540533ms        347
shallow     11.866us   12.966us   15.768us   48.641us  190.039us  2.202726ms        763
nested      35.446us   38.113us    54.97us  240.922us  387.761us  9.477178ms       2242
```

17 ns against 211.5 us: four orders of magnitude, and the smallest p50 is 347
ticks wide. These are the engine.

`tiny` is `{ users { id } }`, `shallow` adds two more scalars, `nested` is
`{ users { id friends { id name } } }` over 100 users with 2 friends each.
Single caller, in process, plan cache warm.

The tail is real and worth reading: p99 is 6x p50 on `tiny` and the maximum is
three orders of magnitude above it. That is the garbage collector, not a
scheduling bug -- these are 200 000 back-to-back allocations of response
buffers with no think time, which is the worst case for it.

Concurrency costs the tail far more than the median:

| callers | tiny p50 | tiny p99 | nested p50 | nested p99 |
|---:|---:|---:|---:|---:|
| 1 | 5.9us | 36us | 38.1us | 241us |
| 4 | 7.6us | 164us | 52.6us | 449us |
| 20 | 12.8us | 1.69ms | 125.6us | 5.00ms |

At 20 callers on 20 cores the machine is saturated *twice over*: each request
also schedules its own fields concurrently, up to `WithMaxConcurrency`, which
defaults to 4 x GOMAXPROCS. Twenty concurrent callers is not twenty cores of
work, and the p99 says so. Treat the single-caller column as the engine's
latency and the rest as this machine under load.

**These are in-process numbers.** `k6/README.md` already established that a
loopback round trip (66-118 us) is larger than anything the transports separate,
so a percentile measured over HTTP would be a percentile of the socket.

## Schema build past 1 600 types

`docs/operations.md` used to say this curve was unknown. Measured on
2026-09-22 with a synthetic schema of six-field types, varying the type count
and the width of the Query root independently:

| types | Query fields | build | retained | allocs |
|---:|---:|---:|---:|---:|
| 1 600 | 1 600 | 15.7 ms | 10.8 MB | 202 048 |
| 4 800 | 4 800 | **77.9 ms** | 33.2 MB | 602 241 |
| 1 600 | 100 | 10.1 ms | 9.6 MB | 181 021 |
| 4 800 | 100 | **34.1 ms** | 29.2 MB | 536 373 |

**The variable is the width of the widest type, not the number of types.**
Three times the types costs 4.96x the time with a Query field per type, and
3.38x with the root capped at 100 -- and at a fixed 4 800 types, narrowing the
root alone takes 77.9 ms to 34.1 ms. Allocations are linear throughout
(202k to 602k for 3x the types), so the extra time is not allocation.

About half of either figure is `gqlparser.LoadSchema`: 34.1 ms of the wide
77.9, 13.1 ms of the narrow 34.1. Its `FieldList.ForName` is a linear scan, so
validating a k-field type is O(k^2), which is the shape observed. **GC is not
involved** -- `GOGC=off` changes the 4 800-type figure by under 1%, which was
worth testing because "big heap, super-linear time" looks like GC and is not.

It matters because an ORM-generated schema puts one Query field per entity.
The mitigation is schema design -- namespace the root instead of flattening it
-- and `BenchmarkSchemaBuildWideRoot` against `BenchmarkSchemaBuildNarrowRoot`
is there so the claim is re-measurable rather than remembered.

## Introspection, and a pooling cliff at 3.3 MB

A full introspection query is the most expensive thing either engine serves,
and it is usually reachable without authentication -- GraphiQL, Apollo Studio,
schema registries and codegen tools all send it on connect. Measured against
the same synthetic schema as above:

| types | response | time | B/op | allocs/op |
|---:|---:|---:|---:|---:|
| 400 | 0.33 MB | 2.15 ms | 1.88 MB | 36 815 |
| 1 600 | 1.26 MB | 7.86 ms | 7.32 MB | 142 425 |
| 3 600 | 2.83 MB | 17.7 ms | 16.6 MB | 318 442 |
| 4 200 | 3.30 MB | 23.9 ms | **38.4 MB** | 371 277 |
| 4 800 | 3.77 MB | 29.2 ms | **40.9 MB** | 424 084 |

Time is linear in the schema. The bytes are not, and between 2.83 MB and
3.30 MB of response they more than double for a 17% larger answer. **That is a
cliff, not a curve**: `jsonw`'s pool drops any buffer whose capacity passes
`maxPooledCap` (4 MiB), so the next request rebuilds it from 512 bytes.

The threshold is not where it reads. `append` overshoots, so the capacity for
a given response is:

| response | capacity | pooled |
|---:|---:|:--|
| 3.00 MB | 3.29 MB | yes |
| 3.15 MB | 3.29 MB | yes |
| **3.62 MB** | **4.12 MB** | **no** |
| 5.00 MB | 5.15 MB | no |

**A response over roughly 3.3 MB stops being pooled, not one over 4 MiB.**
Raising `maxPooledCap` to 64 MiB takes the 4 200-type case from 38.4 MB/op to
19.4 and the 4 800-type case from 40.9 to 21.9, and the time with it (23.9 ms
to 20.0, 25.7 to 23.6) -- which is what identifies the cliff as the cause
rather than the response size itself.

**Changed: `maxPooledCap` is 8 MiB.** Interleaved, n=10:

| | before | after | |
|---|---:|---:|---|
| `IntrospectOverPool` B/op | 39.01 MiB | 19.28 MiB | **-50.6%** (p=0.000) |
| `IntrospectOverPool` sec/op | 27.34 ms | 23.10 ms | **-15.5%** (p=0.000) |
| `ExecuteUsers`, `FieldPathBare` | | | unchanged, allocations equal sample for sample |
| `ExecuteConcurrentList` | | | unchanged (p=0.218) |
| `IntrospectPooled` (below the cliff) | | | unchanged (p=0.247) |

The memory argument against raising it is weaker than it first reads, twice
over. `sync.Pool` is emptied by the collector, so a large buffer survives at
most a couple of GC cycles rather than for the life of the process. And the
pool can only hold buffers that were actually created: to have one per P at
8 MiB, a process must have just served that many concurrent 8 MB responses,
which cost the same memory whether or not they are pooled. Raising the cap
defers a release; it does not raise a peak.

**Changed again: `maxPooledCap` is 16 MiB**, because the reasoning above was
wrong in a way the numbers above could not show. 8 MiB was chosen as
"introspection to roughly 8 000 types", extrapolated from 0.79 KB per type on
the synthetic schema. Type count does not predict the response. The real
consumer schema is 5 516 types and introspects to **6.56 MB** -- 1.25 KB per
type, because its types are about twice as wide -- and 6.56 MB reaches a
capacity of 8.05 MiB, one growth step over the cap. Measured on that schema,
the same query repeated six times:

| | 8 MiB cap | 16 MiB cap |
|---|---:|---:|
| first request | 71.4 MB allocated | 71.4 MB |
| every request after | **71.3 MB** | **31.5 MB** |

Flat at 71.3 MB is the signature: the buffer was never reused. `IntrospectOverPool8`
(8 400 synthetic types, 6.58 MB response) reproduces it in-repo. Interleaved, n=12:

| | before | after | |
|---|---:|---:|---|
| `IntrospectOverPool8` B/op | 72.75 MiB | 34.57 MiB | **-52.5%** (p=0.000) |
| `IntrospectOverPool8` sec/op | 79.62 ms | 61.56 ms | **-22.7%** (p=0.000) |
| `IntrospectOverPool8` allocs/op | 740.9k | 740.9k | equal |
| `IntrospectPooled` (below both caps) | | | not distinguishable (p=0.551) |
| `IntrospectOverPool` (below 8 MiB) | | | not distinguishable (p=0.478) |

An 8 MiB cap pools a response up to about 6.4 MB; 16 MiB pools one up to
15.7 MB, because the runtime's growth steps get closer to exact as they get
larger. `TestPoolCapCoversALargeSchemasIntrospection` now pins the **measured**
6.56 MB rather than a figure derived from type count, which is what let the cap
sit one step too low.
