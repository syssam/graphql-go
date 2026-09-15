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
