# compare

graphql-go and gqlgen over **one shared schema, one shared set of Go structs
and one shared dataset**, so a difference in the numbers is a difference
between the engines rather than between two test harnesses.

    go run gen.go -n 200      # generate both engines (~40s at 200 entities)
    go test -run TestEnginesAgree .
    go test -count=5 -run '^$' -bench . -benchmem .

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

both engines are bound to the same structs in `shared/`, `ID` is mapped to
`string` on both sides, and gqlgen's introspection extension is enabled
because the raw executor leaves it off while graphql-go answers introspection
out of the box.

`TestEnginesAgree` then asserts both return **byte-identical JSON** for six
query shapes — leaf fields, a resolver-backed object, a Relay connection, a
nested connection, an owner chain and a mutation. Without it, the benchmarks
below could be comparing different work.

## Results, 200 entities

Go 1.27.1, Windows, i7-12700. Medians of five runs; allocation counts are
deterministic and were identical across runs. The schema is ~1 600 types.

| Operation | graphql-go | gqlgen | graphql-go is |
|---|---:|---:|---:|
| Leaf fields | 925 ns, 16 allocs | 65 704 ns, 975 allocs | **71x** |
| Owner (nested resolver) | 828 ns, 20 allocs | 66 368 ns, 954 allocs | **80x** |
| Connection, 20 rows | 25 024 ns, 233 allocs | 164 477 ns, 2 715 allocs | **6.6x** |
| Nested connection | 115 836 ns, 1 526 allocs | 429 700 ns, 10 756 allocs | **3.7x** |
| Mutation | 734 ns, 17 allocs | 66 447 ns, 926 allocs | **90x** |
| Full introspection | 8.77 ms, 122 666 allocs | 47.8 ms, 1 739 547 allocs | **5.5x** |
| Schema build (start-up) | 20.0 ms, 240 863 allocs | 9.98 us, 9 allocs | **0.0005x** |

## The finding that matters

**gqlgen's per-request cost scales with the size of the schema, even for a
query that touches one object.** graphql-go's does not.

| Leaf query | 2 entities | 200 entities | growth |
|---|---:|---:|---:|
| graphql-go | 1 032 ns, 16 allocs | 925 ns, 16 allocs | **flat** |
| gqlgen | 7 289 ns, 168 allocs | 65 704 ns, 975 allocs | **9x** |

The allocation counts make the point without any timing noise: graphql-go
allocates the same 16 objects whether the schema holds 2 entities or 200,
while gqlgen goes from 168 to 975. At 200 entities gqlgen allocates 113 KB to
answer `{ entity000(id: "3") { id owner { id name } } }`; graphql-go
allocates 1.6 KB.

That is the compiled-plan design, measured: the plan is built once for the
operation, so what a request costs depends on the query rather than on how
many types the schema happens to contain.

## The trade-off, stated plainly

**graphql-go starts about 2 000x slower.** Building the schema takes 20.0 ms
against gqlgen's 9.98 us, because graphql-go binds and validates the whole
type graph at `NewSchema` where gqlgen did that work at code generation time.

For a long-lived server it is paid once and repaid within a few hundred
requests. For a short-lived process — a serverless invocation answering one
query and exiting — it is the dominant cost, and gqlgen wins.

Start-up also grows faster than gqlgen's: 0.42 ms to 20.0 ms from 2 to 200
entities (47x), against 0.37 us to 9.98 us (27x).

## Layout

    compare/
      gen.go              go run gen.go -n 200
      internal/gen/       the generator: schema, shared structs, both engines
      engines.go          the two runners
      compare_test.go     both engines must return identical JSON
      bench_test.go       the same operations against both
      schema/  shared/  gqlgen/  graphqlgo/     generated, git-ignored

## Caveats

Timings are medians of five runs on an otherwise idle machine; individual runs
varied by up to about 10%, and a warm machine inflates both engines by 20-50%,
so treat single samples with suspicion. Allocation figures are deterministic.

The build-cost comparison — generation time, peak memory, compile and
incremental rebuild — lives separately in
[`docs/benchmarks.md`](../docs/benchmarks.md); this module measures execution.
