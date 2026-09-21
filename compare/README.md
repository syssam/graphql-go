# compare

graphql-go and gqlgen over **one shared schema, one shared set of Go structs
and one shared dataset**, so a difference in the numbers is a difference
between the engines rather than between two test harnesses.

    go run gen.go -n 200                          # generate both engines (~40s)
    go test -run TestEnginesAgree .               # they must agree first
    go test -count=5 -run '^$' -bench . -benchmem .

    go run ./graphqlgo/cmd/graphqlgo-server -addr :18080
    go run ./gqlgen/cmd/gqlgen-server   -addr :18081

Generated code is not committed: gqlgen emits 429 834 lines at 200 entities.

## What makes it a fair comparison

The engines model resolvers differently by default, and comparing them as they
come out of the box measures two different workloads:

- **gqlgen** puts composite fields in the model struct (`Owner *Entity001`)
  and serves them from struct data. Its `ResolverRoot` asks only for `Query()`
  and `Mutation()`.
- **graphql-go** emits models holding only leaf fields and makes every
  composite field a resolver method.

So gqlgen is told, per field, to resolve instead:

    models:
      Entity000:
        fields:
          owner:    { resolver: true }
          children: { resolver: true }

both engines bind to the same structs in `shared/`, `ID` maps to `string` on
both sides, and gqlgen's introspection extension is enabled because the raw
executor leaves it off while graphql-go answers introspection out of the box.

`TestEnginesAgree` and `TestHTTPEnginesAgree` then assert both return
**byte-identical JSON** for six query shapes, in process and again over HTTP.
Without that, everything below could be comparing different work.

## Results

200 entities (~1 600 types), Go 1.27.1, Windows, i7-12700, medians of five
runs taken in one sitting. Allocation counts are deterministic.

### In process

Re-measured 2026-09-21, both engines in one run, after the allocation work in
`32d994e..d542c6b`. gqlgen's allocation counts came back within 0.03% of the
previous reading (975, 954, 926, 2 715, 10 756, 1 740 242), which is what says
the fixture and the method are the same and only this side moved. The timings
are from a busier machine than the earlier sitting, so compare them with each
other rather than with the numbers this table used to carry; the allocation
columns are deterministic and carry no such caveat.

| Operation | graphql-go | gqlgen | time ratio | alloc ratio |
|---|---:|---:|---:|---:|
| Leaf fields | 1 149 ns, 12 allocs | 81 920 ns, 975 allocs | **71x** | **81x** |
| Owner (nested resolver) | 1 147 ns, 14 allocs | 77 984 ns, 954 allocs | **68x** | **68x** |
| Mutation | 980 ns, 13 allocs | 80 931 ns, 926 allocs | **83x** | **71x** |
| Connection, 20 rows | 32 920 ns, 185 allocs | 219 159 ns, 2 715 allocs | **6.7x** | **15x** |
| Nested connection | 124 723 ns, 1 199 allocs | 531 987 ns, 10 756 allocs | **4.3x** | **9.0x** |
| Full introspection | 12.5 ms, 117 528 allocs | 62.8 ms, 1 740 242 allocs | **5.0x** | **15x** |

The HTTP and saturation tables below are from the earlier sitting and have not
been re-measured; their graphql-go allocation columns are therefore high by
roughly the same four allocations a request that this one lost.

### Over HTTP, one request at a time

| Operation | graphql-go | gqlgen | ratio |
|---|---:|---:|---:|
| Leaf fields | 83.5 us, 114 allocs | 194.3 us, 1 079 allocs | **2.3x** |
| Owner | 72.6 us, 118 allocs | 182.1 us, 1 057 allocs | **2.5x** |
| Mutation | 67.9 us, 115 allocs | 169.3 us, 1 030 allocs | **2.5x** |
| Connection | 120.1 us, 335 allocs | 348.6 us, 2 830 allocs | **2.9x** |
| Nested connection | 297.6 us, 1 632 allocs | 725.1 us, 10 899 allocs | **2.4x** |

### Over HTTP, saturated

| | graphql-go | gqlgen | ratio |
|---|---:|---:|---:|
| GOMAXPROCS clients | 16.3 us, 319 allocs | 73.4 us, 2 763 allocs | **4.5x** |
| Sustained, 32 clients | 67 109 req/s | 12 071 req/s | **5.6x** |

### Start-up and memory

| | graphql-go | gqlgen | |
|---|---:|---:|---|
| Schema build | 30.4 ms, 240 863 allocs | 13.9 us, 9 allocs | **2 180x slower** |
| Heap retained by the schema | **11.0 MB** | 0.04 MB | **278x more** |
| Binary size | 51.1 MB | 58.5 MB | 1.15x smaller |
| Server RSS, idle | 46.4 MB | 21.9 MB | 2.1x more |
| Server RSS, after 400 requests | 55.3 MB | 31.7 MB | 1.7x more |

## What the numbers say

**gqlgen's per-request cost scales with the size of the schema, even for a
query that touches one object.** graphql-go's does not:

| Leaf query | 2 entities | 200 entities |
|---|---:|---:|
| graphql-go | 1 032 ns, **16 allocs** | 1 103 ns, **16 allocs** |
| gqlgen | 7 289 ns, **168 allocs** | 98 323 ns, **975 allocs** |

The allocation counts carry the point without any timing noise: graphql-go
allocates the same 16 objects whether the schema holds 2 entities or 200,
while gqlgen goes from 168 to 975. That is the compiled-plan design measured —
the plan is built once for the operation, so a request costs what the query
costs, not what the schema costs.

**Transport compresses the advantage, saturation restores it.** A loopback
round trip costs roughly 70 us, which dwarfs the 1 us graphql-go needs for a
leaf query, so one-at-a-time HTTP shows 2.3x rather than 89x. Under
concurrency the round trips overlap, per-request CPU decides throughput again,
and the ratio returns to 4.5-5.6x. The in-process figure is an upper bound and
the sequential HTTP figure a lower one; a loaded server sits between them.

**The cost is start-up and memory.** graphql-go binds and validates the whole
type graph at `NewSchema` where gqlgen did that at code generation time, and
then holds it: 11 MB of retained heap against 0.04 MB, and roughly 24 MB more
resident per process. A long-lived server repays the 30 ms build within a few
hundred requests and will not notice it again. A short-lived process answering
one query will, and so will a deployment that is memory-bound rather than
CPU-bound: at 55 MB against 32 MB, fewer replicas fit per node.

## Caveats

Timings are medians of five runs on an otherwise idle machine; a warm machine
inflates both engines by 20-50%, so treat any single sample with suspicion and
compare versions with `benchstat` rather than by eye. Allocation counts and
retained heap are stable and are the figures to trust.

No latency percentiles are reported. This machine's monotonic clock has a
granularity of about 522 us — 99 999 of 100 000 back-to-back `time.Since`
calls return exactly zero — so a request served in 70 us cannot be timed
individually, and the *faster* engine accumulates more unmeasurable samples,
biasing any percentile toward gqlgen. Throughput spans thousands of ticks and
is unaffected. Tail latency needs a platform with a finer clock, and any
external load tool inherits the same limit when run here.

`handler.NewDefaultServer` is heavier than `gqlhttp.New` — it adds
introspection, automatic persisted queries and a multipart transport — so part
of the HTTP gap is gqlgen's default server rather than its engine.

## Layout

    compare/
      gen.go              go run gen.go -n 200
      internal/gen/       the generator: schema, shared structs, both engines
      engines.go          the two runners
      compare_test.go     both engines must return identical JSON
      bench_test.go       the same operations in process
      http_test.go        the same operations through a real HTTP server
      load_test.go        throughput under concurrent clients
      memory_test.go      heap retained by a built schema
      graphqlgo/
        cmd/graphqlgo-server/   one process per engine, so a load test does
        gen/                    not have both sharing a GC and a CPU
      gqlgen/
        cmd/gqlgen-server/
        gen/
      schema/  shared/    generated, git-ignored

Generated output lives under each engine's `gen/`, which is what the generator
deletes and git ignores. Hand-written code sits beside it and survives
regeneration.

Build cost — generation time, peak memory, compile and incremental rebuild —
is measured separately in [`docs/benchmarks.md`](../docs/benchmarks.md).
Specification conformance is in
[`docs/graphql-http-audit.md`](../docs/graphql-http-audit.md).
