# Engine comparison: graphql-go vs gqlgen

Measured on 2026-09-14, Windows, Go 1.27.1, 12th Gen Intel Core i7-12700.
Re-run with `cd benchmarks && go test -run '^$' -bench . -benchmem -count=5`.

## What is compared

Same SDL, same 100 `User` values, same Go struct, same queries:

```graphql
type User { id: ID! name: String! email: String! friends: [User!]! }
type Query { users: [User!]! }
```

- `{ users { id name email } }` — 100 objects, three scalars each
- `{ users { id friends { id name } } }` — 100 objects plus 200 friend objects

Both engines are warmed (one execute) so graphql-go's plan cache and gqlgen's
document LRU (1024) are hot. `Query.users` is a resolver; `User` fields are
plain struct access. No HTTP, no DataLoader, no complexity interceptor.

This is **not** the Phase 4 200-entity codegen/compile comparison. It is a
steady-state runtime comparison of the two executors.

## Results (median of 5)

| Query | Engine | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| Shallow | graphql-go | 12 858 | 5 730 | 116 |
| Shallow | gqlgen 0.17.95 | 221 737 | 248 505 | 4 976 |
| Nested | graphql-go | 44 174 | 30 680 | 716 |
| Nested | gqlgen 0.17.95 | 689 574 | 596 058 | 12 180 |

Approximate ratio (gqlgen / graphql-go): **~17×** time and **~40×** bytes on
the shallow list; **~16×** time and **~19×** bytes on the nested list.

## Why the gap is this large

gqlgen still builds a `graphql.Marshaler` tree per field, per request, even
with a cached query document. graphql-go compiles a typed plan once and writes
JSON into a pooled buffer. That is the design bet; these numbers are the first
measurement of it against gqlgen on identical data.

Treat them as a lower bound on gqlgen cost: `executor.New` is already the
light path (no HTTP transports, no APQ, no tracing extension). A default
`handler.NewDefaultServer` would allocate more.

## Reproduce

```sh
cd benchmarks
go test -count=1 .
go test -run '^$' -bench . -benchmem -count=5
go generate   # only if schema.graphql or gqlgen.yml changed
```

Compare two versions with `benchstat` rather than by reading medians. It reports a
confidence interval and says whether a difference is significant, which matters here:
a warm machine has moved these numbers by 20-77%, in both directions.

```sh
go test -count=10 -run '^$' -bench . -benchmem > old.txt
go test -count=10 -run '^$' -bench . -benchmem > new.txt
benchstat old.txt new.txt
```

Allocation counts are deterministic and are the figure to trust when timings are noisy.

---

# Build cost: gqlc vs gqlgen

Measured on 2026-09-14, Windows, Go 1.27.1, 12th Gen Intel Core i7-12700.
Re-run with `cd benchmarks && go run ./cmd/buildbench -n 50,200 [-split]`.

This is the Phase 4 comparison the runtime section defers: the cost that
motivated the project (design section 1).

## Method

`buildbench` generates a synthetic ORM-shaped schema of N entities -- each
with a Relay connection, a `WhereInput` filter, an order input and CRUD
mutations, linked to two neighbours -- in two layouts:

- **flat**: one SDL file, so gqlc puts everything in one group.
- **split**: one SDL file per entity, so gqlc emits one package per entity.
  This is the Velox-style layout the design targets.

For each engine, in its own temporary module with its own `GOCACHE`:

1. Build the generator and a stub importing the runtime (untimed, to warm the
   cache).
2. Time the generator. Peak memory comes from a Windows job object, so the
   child compiler processes `go/packages` spawns are counted.
3. Empty the cache, re-warm the dependencies, then time `go build ./graph/...`.
   The reset matters: gqlgen type-checks generated code during generation and
   leaves it compiled in the cache, so without it gqlgen appears to build in
   260 ms at any schema size. gqlc never loads Go packages and gets no such
   head start.
4. Add a field to one entity's SDL, regenerate, rebuild. **Regen** and
   **rebuild** are reported separately because gqlgen does its type-checking
   in the regen step; only the sum is comparable.

The 200-entity split rows are medians of three runs (gqlc) and two runs
(gqlgen); the rest are single samples. Generate, memory, LOC, rebuild and
edit -> built are stable across runs -- gqlc's edit -> built spanned
9.61-10.15 s, gqlgen's 32.10-32.19 s. The compile column is not: repeats of
the same case spanned 22.7-26.9 s for gqlc and 24.8-28.3 s for gqlgen, partly
because a machine warmed by earlier runs clocks down. Read compile as a
range. Numbers here were taken on a cool machine where possible.

## Results

| Entities | Layout | Engine | Generate | Peak RSS | Pkgs | LOC | Compile | Regen | Rebuild | Edit->built |
|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 50 | flat | gqlc | 287 ms | 23 MB | 2 | 8 851 | 4.57 s | 56 ms | 4.58 s | **4.64 s** |
| 50 | flat | gqlgen | 7.75 s | 1.32 GB | 2 | 100 345 | 4.57 s | 8.15 s | 259 ms | **8.41 s** |
| 50 | split | gqlc | 390 ms | 22 MB | 103 | 11 750 | 5.26 s | 171 ms | 2.18 s | **2.35 s** |
| 50 | split | gqlgen | 8.26 s | 1.36 GB | 2 | 100 995 | 4.32 s | 8.05 s | 265 ms | **8.31 s** |
| 200 | flat | gqlc | 430 ms | 82 MB | 2 | 35 084 | 27.2 s | 182 ms | 33.25 s | **33.43 s** |
| 200 | flat | gqlgen | 40.73 s | 4.62 GB | 2 | 392 245 | 28.56 s | 36.94 s | 305 ms | **37.25 s** |
| 200 | split | gqlc | 1.47 s | 72 MB | 403 | 42 485 | 29.91 s | 1.06 s | 10.3 s | **11.37 s** |
| 200 | split | gqlgen | 39.29 s | 4.36 GB | 2 | 394 845 | 31.39 s | 38.39 s | 426 ms | **38.82 s** |

gqlgen ignores the layout: its output is one `generated.go` plus one model
package either way.

### Reading the 200-entity rows

The 50-entity rows are older. The 200-entity rows were re-measured in one
sitting after phase 4, both engines back to back, which is the only way the
comparison means anything: every absolute number here is larger than the
figures this document first carried, gqlgen's included -- its generation went
from 31.4 s to 40.7 s on identical input. That is the machine, not the code.
**Compare the ratios, not the seconds.**

| At 200 entities | flat | split |
|---|---:|---:|
| Generation | **95x** faster | **27x** faster |
| Peak RSS | **56x** less | **60x** less |
| Generated lines | **11x** fewer | **9.3x** fewer |
| Edit to rebuilt | 1.11x faster | **3.4x** faster |

Generation speed is unchanged from the original measurement at 95x. Peak RSS
was 105x and is now 56x, and that one is the code rather than the machine:
`codegen` imports `golang.org/x/tools/go/packages` for auto-bind, which links
into `gqlc` whether or not auto-bind is used, taking the binary from 5.4 MB to
9.7 MB and roughly doubling its resident set. Generated output is unaffected --
byte-identical to the previous commit for the same schema, verified by
generating with both binaries and diffing.

It is worth being plain about what that trade bought: a feature many users will
never enable costs every user of the CLI about 40 MB of peak memory. It is the
clearest argument yet for `codegen` becoming its own module, and it is recorded
here rather than absorbed quietly into a smaller headline number.

## Confirmed

**Generation time and memory, decisively.** At 200 entities gqlc generates in
331 ms using 44 MB; gqlgen takes 31.4 s and peaks at **4.64 GB** -- 95x the
time and 108x the memory. gqlgen grows superlinearly (2.9 s, 3.3 s, 7.8 s,
31.4 s for 5/10/50/200 entities) while gqlc stays near-flat (184 ms to 331 ms
for a 40x larger schema). Design section 1 claimed gqlgen's whole-module type
loading costs "minutes and several GB": several GB is now measured fact.

**Generated volume: 8-11x smaller** (8.5x split, 11.2x flat, at 200 entities).

**End to end, SDL to compiled code**, at 200 entities in the split layout:
10.0 s against 32.1 s, **3.2x**.

## Not confirmed

**Per-group splitting bought parallelism but not incrementality, until the
models were split too.** With all models in one shared package, splitting 200
entities into 203 packages cut the cold compile from 26.0 s to 17.8 s, but a
one-entity edit still rebuilt everything (18.26 s against a 17.76 s full
compile). Every generated entity package imported `graph/model`, so touching
one entity changed that package's export data and invalidated all 200
dependents.

Emitting one model package per group (2026-09-14) fixes it:

| 200 entities, split | shared model | per-group model |
|---|---:|---:|
| Packages | 203 | 403 |
| Cold compile | 17.76 s | 22.7-26.9 s (median 24.0) |
| Rebuild after one-entity edit | 18.26 s | **8.4-8.9 s (median 8.8)** |
| Edit -> built | 18.81 s | **9.6-10.2 s (median 10.0)** |

At 50 entities the same change takes edit -> built from 4.16 s to 2.35 s.

The trade is real and worth stating: the incremental rebuild roughly halves
(2.2x at 200 entities, 1.9x at 50), and the cold compile gets worse by
anywhere from 18% (50 entities) to 51% (200 entities, slowest run), because
the extra packages cost per-package overhead that a single model package did
not. A dev loop pays the
cold build once and the incremental cost on every edit, so this is the right
side of the trade for the design's goal; CI cold builds get slower.

Cold-compile figures here span two runs and vary by about 18% between them;
the rebuild figures are stable. Treat the compile column as a range.

Against gqlgen at 200 entities, this moves **edit -> built from 1.7x to about
3.2x** (median 10.0 s against 32.1 s), while gqlc's cold compile is now
roughly level with gqlgen's rather than ahead of it.

**Compile time per line is still much worse.** gqlc's 46 700 lines take
22.7-26.9 s; gqlgen's 394 845 lines take 24.8 s. Eight times less code
compiles no faster, so graphql-go's generated code costs roughly 8x more per
line -- the price of generic instantiation. The README's original "a
200-entity schema compiles in seconds, not minutes" is still not supported.

## Note

The split layout did not compile at all until 2026-09-14: root fields with no
arguments were emitted as pure field accessors on a `model.Query` /
`model.Mutation` that is never generated. Found by this benchmark, fixed in
`codegen.fieldKind`, regression test in `codegen/generate_test.go`.

---

# Transport cost: net/http, Echo, Fiber native, Fiber adaptor

Measured on 2026-09-15, Windows, Go 1.27.1, 12th Gen Intel Core i7-12700,
Echo v5.3.1, Fiber v3.5.0.
Re-run with:

```sh
cd benchmarks
go test -run '^$' -bench BenchmarkTransport -benchmem -count=10 > new.txt
benchstat new.txt    # golang.org/x/perf/cmd/benchstat
```

The `±` columns below are benchstat's; `go test` alone does not produce them,
and a single sample of any timing here is not worth reading.

This section exists to test one claim `transport/gqlfiber`'s package
documentation makes for itself: that implementing the transport natively on
fasthttp earns its complexity over wrapping `gqlhttp` in Fiber's
`middleware/adaptor`. The `FiberAdaptor` row is the falsifier.

## What is compared

Four transports over the same executor, the same schema and the same query:

| Row | What |
|---|---|
| `NetHTTP` | `gqlhttp` on a `net/http` `ServeMux` |
| `Echo` | `gqlecho` on Echo v5 |
| `FiberNative` | `gqlfiber` on Fiber v3 |
| `FiberAdaptor` | `gqlhttp` wrapped in `adaptor.HTTPHandler`, on Fiber v3 |

Two harnesses, because they do not measure the same thing:

- **in-process** invokes each handler against a response object reused across
  iterations (a `bytes.Buffer` behind an `http.ResponseWriter`, a
  `fasthttp.RequestCtx` for the Fiber rows), so the allocations reported are
  the transport's and not the harness's.
- **loopback** drives a real listener per framework through one shared
  `http.Client`, serially over one kept-alive connection. Connection and
  header handling -- the part fasthttp exists for -- happens only here.

Two payload sizes, because per-request transport overhead is a fixed cost that
a large response hides:

- **Tiny** -- `{ __typename }`, a 30-byte response.
- **List** -- `{ users { id name email } }`, 100 objects, the same query the
  engine comparison above calls *Shallow*.

`TestTransportRowsAgree` asserts all four rows return byte-identical response
bodies, so no row is cheaper for having done less.

## Results: allocations per operation

Allocation counts held to the unit across two full `-count=10` runs: benchstat
reports `~ (p=1.000)` for every row but loopback Tiny/Echo, whose median moved
112 → 111. Two loopback cells (Tiny/Echo, List/FiberNative) also carry a ±1%
spread within a run; every in-process cell is ±0%. They are the figure to read.

| Harness | Query | NetHTTP | Echo | FiberNative | FiberAdaptor |
|---|---|---:|---:|---:|---:|
| in-process | Tiny | 20 | 21 | **18** | 40 |
| in-process | List | 127 | 128 | **125** | 147 |
| loopback | Tiny | 110 | 112 | **84** | 110 |
| loopback | List | 222 | 224 | **192** | 218 |

The loopback rows include the client's own allocations, identical for every
row, so only the differences between rows mean anything there.

**The adaptor costs a flat +22 allocations per request in-process, +26 end to
end, at both payload sizes.** End to end that is very nearly the whole of what
fasthttp was saving. On the tiny response `FiberNative` is 26 allocations per
request cheaper than `NetHTTP` (84 against 110) and `FiberAdaptor` gives all 26
back, landing exactly on `NetHTTP`. On the list response the saving is 30 (192
against 222) and the adaptor gives back 26 of them, landing at 218.

**Echo's passthrough costs one to two allocations per request** -- 21 against
20, 128 against 127 -- most of which is the `statusRecorder` `gqlecho.serve`
wraps the writer in. There is nothing else in that transport.

## Results: bytes and time, in-process

Both `-count=10` runs are shown. Timings on this machine drift between
sittings by more than most of the differences below, so a single column would
invite conclusions the second column does not support.

| Query | Row | sec/op (run A) | sec/op (run B) | B/op (A) | B/op (B) |
|---|---|---:|---:|---:|---:|
| Tiny | NetHTTP | 2.016µ ± 96% | 2.787µ ± 20% | 1.396Ki | 1.397Ki |
| Tiny | Echo | 2.550µ ± 9% | 2.806µ ± 21% | 1.422Ki | 1.422Ki |
| Tiny | FiberNative | 2.110µ ± 7% | 2.035µ ± 6% | 893 | 893 |
| Tiny | FiberAdaptor | 8.875µ ± 11% | 8.475µ ± 51% | 4.090Ki | 4.094Ki |
| List | NetHTTP | 20.48µ ± 16% | 20.72µ ± 10% | 6.451Ki | 6.449Ki |
| List | Echo | 19.56µ ± 7% | 20.21µ ± 15% | 6.482Ki | 6.481Ki |
| List | FiberNative | 21.08µ ± 14% | 18.07µ ± 93% | 5.931Ki | 5.926Ki |
| List | FiberAdaptor | 29.60µ ± 5% | 29.10µ ± 13% | 9.336Ki | 9.322Ki |

Bytes per operation are stable across runs -- benchstat calls every cell above
`~` except `List/FiberNative` at -0.09%. Timings are not, and two cells
(`Tiny/NetHTTP` at ±96% in run A, `List/FiberNative` at ±93% in run B) carry a
single outlier sample each.

**The one timing result that reproduces is the tiny-payload gap.** Native
against adaptor: **4.21x in run A, 4.16x in run B** (6.8µs and 6.4µs of
absolute gap). Bytes: **4.7x**, stable.

**The list-payload time ratio does not reproduce well**: 1.40x in run A,
1.61x in run B -- 15% apart, because its subtrahend `List/FiberNative` is one
of the two outlier-carrying cells. Read it as "somewhere around 1.4-1.6x",
not as 1.4x. The allocation ratio on the same row is exactly 1.18x in both
runs.

The spread between payload sizes is the whole point of running two: the
adaptor's cost is fixed per request, so it dominates a small response and is
diluted by a large one. On a realistic list query the native path is worth
1.18x the allocations and roughly 1.4-1.6x the time; on a small or errored
response it is worth about four times on both.

## Why: what is measured, what is read from source, what is inferred

The claim in `gqlfiber`'s package comment is that the adaptor "would rebuild a
synthetic `*http.Request` per call". That is true, it is not the whole cost,
and the allocation half of the accounting is much firmer than the timing half.
`BenchmarkTransportAdaptorOverhead`, both runs:

| | sec/op (A) | sec/op (B) | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `ConvertRequest` (build the synthetic request) | 869.4n ± 5% | 764.1n ± 6% | 674 | 8 |
| `GoroutineHandoff` (empty child goroutine) | 591.6n ± 6% | 508.1n ± 10% | 16 | 1 |
| `HandlerOnGoroutine` (the NetHTTP row's work, moved to a fresh goroutine) | 4.235µ ± 12% | 3.605µ ± 6% | 1.430Ki | 21 |

`fasthttpadaptor.NewFastHTTPHandler` runs the `net/http` handler **on a new
goroutine per request** and blocks the caller on a channel until it reports
back, which is how it can react to a `Flush` or a `Hijack` it cannot know
about in advance.

### Where the 22 extra allocations go

This part is solid: every input is a `±0%` count, and 19 of the 22 are
accounted for.

| | allocs | source |
|---|---:|---|
| `ConvertRequest` builds the synthetic `*http.Request` | 8 | measured |
| the `go func(){...}` closure | 1 | measured |
| `acquireWriter`: `io.Pipe()` (the `pipe`, its three channels, the `PipeReader` and `PipeWriter`) | 6 | read from `fasthttpadaptor/adaptor.go:223` |
| `acquireWriter`: `make(http.Header)`, `modeCh`, `streamReady`, the `writer` itself | 4 | same |
| `r.WithContext` and the header copy-back | ~3 | inferred, the remainder |

Despite the name, `acquireWriter` pools nothing -- `releaseWriter` returns only
a byte buffer -- so those 10 allocations are per-request garbage. **They exist
solely to detect a `Flush` or a `Hijack`**, which is the same requirement that
forces the goroutine.

Counting only rows above that a reader can point at: **10 of the 22 go to that
machinery, 11 with the goroutine's own closure -- half the adaptor's allocation
overhead, and the largest identifiable group in the table.** A `gqlhttp`
response streams nothing that needs any of it. (The ~3 inferred allocations are
deliberately not counted in: one of them is `r.WithContext`, which has nothing
to do with `Flush` or `Hijack`.)

### Where the time goes, and why this is a range

The tiny-payload gap itself reproduces: **6.8µs (run A), 6.4µs (run B)**. Its
decomposition does not.

| | run A | run B |
|---|---:|---:|
| total gap, `FiberNative` → `FiberAdaptor` | +6.8µs | +6.4µs |
| `ConvertRequest` | +0.87µs | +0.76µs |
| goroutine premium (`HandlerOnGoroutine` − `NetHTTP`, same run) | +2.2µs | +0.8µs |
| residual | ~3.7µs | ~4.9µs |

**The two runs do not agree on which mechanism costs more.** Run A puts the
goroutine at 2.6x the request rebuild; run B puts the two level. The goroutine
premium is a subtraction between two independently noisy benchmarks, and its
subtrahend `InProcess/Tiny/NetHTTP` is the ±96% cell. State it as **+0.8µs to
+2.2µs** and no more: on this data the honest answer is that rebuilding the
request and spawning the goroutine are the same order of magnitude, and which
one leads is not established.

Two further reasons to treat that premium as an upper bound:

- The `HandlerOnGoroutine` control uses an **unbuffered** channel, so its child
  parks until the parent receives. The adaptor's `modeCh` is `make(chan int, 1)`
  and its child sends under a `select`/`default`, so it never blocks. The
  control models a stricter handoff than the adaptor performs.
- `GoroutineHandoff` (an empty child, ~0.5-0.6µs) and `HandlerOnGoroutine` (a
  child doing real work) differ by a factor of four to seven, so the cost is
  sensitive to what the child does -- which is a reason to distrust any single
  figure for it, not just to prefer the larger one.

The residual (3.7-4.9µs, the largest single chunk in both runs) is **not
measured**: it covers `acquireWriter`'s pipe and channels, copying the
synthetic writer's headers and body back into the fasthttp response,
`r.WithContext`, and the collector's share of 4.7x the garbage. That last item
belongs to the time residual only -- garbage collection cannot add to an
allocation count, and the alloc and time residuals above are deliberately kept
apart for that reason.

So: the direction of the design claim holds decisively, and its stated reason
is one of at least three comparable costs. Beyond that, the two columns part
company, and the conclusion should be read off the one that reproduces:

- **Allocations** -- half the overhead (11 of 22) buys `Flush`/`Hijack`
  detection that a GraphQL response never uses. Every input to that is a ±0%
  count or a line of `acquireWriter`.
- **Time** -- the residual outweighs both measured components in both runs, but
  it is a mixture this benchmark does not separate: `acquireWriter`'s pipe and
  channels are only one of its four listed contributors, and it should not be
  given a single label.

Recorded at this length because the reason is what a reader would otherwise
generalise from, and because a plausible-looking attribution is exactly how
someone ends up optimising the wrong thing.

### A behavioural divergence, not a performance one

Worth knowing before choosing the adaptor for anything: `fasthttpadaptor`
passes the `*fasthttp.RequestCtx` itself as the adapted request's
`context.Context`, and fasthttp documents `RequestCtx.Done()` as returning the
**server's** done channel -- "`RequestCtx.s.done` is only closed when the
server is shutting down" (`fasthttp@v1.74.0/server.go:3017`). So `gqlhttp`
behind `adaptor.HTTPHandler` gets a request context that **never cancels when
the client disconnects**, and symmetrically, every in-flight adapted request
sees `ctx.Err() != nil` the moment the server begins shutting down. Neither
matches `net/http`, and no test notices unless it disconnects mid-request.
`gqlfiber` derives a cancellable context per request instead; see
`requestContext` in `transport/gqlfiber/gqlfiber.go`.

## What the loopback timings could not settle

Loopback `ns/op` on this machine cannot rank these transports. Two identical
`-count=10` runs, back to back, no code change:

| Row (loopback) | run A | run B | benchstat |
|---|---:|---:|---|
| Tiny/NetHTTP | 109.46µ ± 15% | 93.97µ ± 14% | -14.15% (p=0.009) |
| Tiny/Echo | 126.19µ ± 4% | 89.39µ ± 2% | -29.16% (p=0.000) |
| Tiny/FiberNative | 91.74µ ± 14% | 81.62µ ± 4% | -11.02% (p=0.023) |
| Tiny/FiberAdaptor | 108.73µ ± 25% | 87.98µ ± 8% | -19.09% (p=0.000) |
| List/NetHTTP | 180.5µ ± 9% | 128.9µ ± 7% | -28.56% (p=0.000) |
| List/Echo | 239.3µ ± 24% | 128.3µ ± 10% | -46.40% (p=0.000) |
| List/FiberNative | 164.0µ ± 30% | 110.7µ ± 6% | -32.48% (p=0.000) |
| List/FiberAdaptor | 153.7µ ± 13% | 121.7µ ± 4% | -20.83% (p=0.000) |

Every row "improved significantly" between two runs of the same binary. The
row ordering flipped too: on the List payload run A put `FiberAdaptor` ahead
of `FiberNative`, run B reversed it. A round trip costs 80-240µs here and the
transport difference is single-digit microseconds inside it, so the signal is
below the machine's drift. `FiberNative` was fastest in three of the four
loopback cases, which is suggestive and nothing more.

The in-process rows drifted too -- the `AdaptorOverhead` rows all moved 12-15%
with p ≤ 0.001 between the same two runs -- which is why they are published
above as two columns rather than one. The allocation columns moved in one cell
out of twenty. That asymmetry is the reason this section leads with
allocations, and the reason the only timing conclusion it draws is the
tiny-payload ratio, which reproduced to within 1%.

## Caveats

- The in-process harness gives the Fiber rows a `fasthttp.RequestCtx` and the
  net/http rows a `ResponseWriter` over a reused buffer. They are as close as
  two different stacks can be made, but they are not the same object, so read
  `FiberNative` against `NetHTTP` in-process as indicative. `FiberNative`
  against `FiberAdaptor` shares a harness exactly, and that is the comparison
  this section exists for.
- The loopback harness is serial over one kept-alive connection. It does not
  exercise connection acceptance or concurrency, where fasthttp's remaining
  advantages are.
- The adaptor's cost is split three ways above with three different grades of
  evidence: the allocation counts are measured, `acquireWriter`'s ten are read
  off its source, and the ~3 remaining allocations and the whole time residual
  are inferred. The `HandlerOnGoroutine` control is also stricter than the
  adaptor's own handoff (unbuffered rendezvous against a buffered
  non-blocking send), so its figure is an upper bound. Matching it exactly
  would need another measurement sitting and is left undone.
- This measures only the performance half of `gqlfiber`'s rationale. The other
  half -- that the adaptor cannot carry a WebSocket, and hands the wrapped
  handler a request context that never cancels on client disconnect -- is not
  a number.
