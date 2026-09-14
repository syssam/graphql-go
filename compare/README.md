# compare

graphql-go and gqlgen over **one shared schema, one shared set of Go structs
and one shared dataset**, so a difference in the numbers is a difference
between the engines rather than between two test harnesses.

    go run gen.go -n 200      # generate both engines (takes ~40s at 200)
    go test -run TestEnginesAgree .
    go test -run '^$' -bench . -benchmem .

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

and both engines are bound to the same structs in `shared/`. `ID` is mapped to
`string` on both sides so the struct fields are identical.

`TestEnginesAgree` then asserts both return **byte-identical JSON** for six
query shapes — leaf fields, a resolver-backed object, a Relay connection, a
nested connection, an owner chain and a mutation. Without that, any benchmark
below could be comparing different work.

## Results, 200 entities

Go 1.27.1, Windows, i7-12700. The schema is ~1 600 types: each entity has a
Relay connection, a filter input, an order input and CRUD mutations.

| Operation | graphql-go | gqlgen | graphql-go is |
|---|---:|---:|---:|
| Leaf fields | 1 231 ns, 16 allocs | 93 765 ns, 975 allocs | **76x** |
| Owner (nested resolver) | 986 ns, 20 allocs | 98 627 ns, 954 allocs | **100x** |
| Connection, 20 rows | 30 878 ns, 233 allocs | 217 073 ns, 2 715 allocs | **7.0x** |
| Nested connection | 182 817 ns, 1 526 allocs | 647 907 ns, 10 756 allocs | **3.5x** |
| Mutation | 1 037 ns, 17 allocs | 94 091 ns, 926 allocs | **91x** |
| Full introspection | 12.7 ms, 122 666 allocs | 61.1 ms, 1 739 547 allocs | **4.8x** |
| Schema build (start-up) | 35.5 ms, 240 863 allocs | 13.8 us, 9 allocs | **0.0004x** |

## The finding that matters

**gqlgen's per-request cost scales with the size of the schema, even for a
query that touches one object.** graphql-go's does not:

| Leaf query | 2 entities | 200 entities | growth |
|---|---:|---:|---:|
| graphql-go | 1 023 ns | 1 231 ns | 1.2x |
| gqlgen | 8 725 ns | 93 765 ns | **10.7x** |

At 200 entities gqlgen allocates 113 KB and 954 objects to answer
`{ entity000(id: "3") { id owner { id name } } }`. graphql-go allocates 1.6 KB
and 20. The allocation counts are identical across runs, so this is
structural, not noise.

That is the whole thesis of the compiled-plan design, measured: the plan is
built once for the operation, so what a request costs depends on the query,
not on how many types the schema happens to contain.

## The trade-off, stated plainly

**graphql-go starts 2 570x slower.** Building the schema takes 35.5 ms against
gqlgen's 13.8 us, because graphql-go binds and validates the whole type graph
at `NewSchema` where gqlgen did that work at code generation time.

For a long-lived server this is paid once and repaid within a few hundred
requests. For a short-lived process -- a serverless invocation that answers
one query and exits -- it is the dominant cost, and gqlgen wins.

## Layout

    compare/
      gen.go              go run gen.go -n 200
      internal/gen/       the generator: schema, shared structs, both engines
      engines.go          the two runners
      compare_test.go     both engines must return identical JSON
      bench_test.go       the same operations against both
      schema/  shared/  gqlgen/  graphqlgo/     generated, git-ignored

## Caveats

Single sample per cell except the leaf and owner rows, which were repeated
three times and were stable to within a few percent. Numbers were taken on an
otherwise idle machine. The build-cost comparison (generation time, peak
memory, compile) lives separately in
[`docs/benchmarks.md`](../docs/benchmarks.md); this module measures execution.
