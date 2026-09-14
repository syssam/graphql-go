# compare

A side-by-side comparison of graphql-go and gqlgen over **one shared schema**:
the synthetic ORM-shaped CRUD schema from `benchmarks/buildbench`, at any
entity count.

Status: **foundation only.** The module, its pinned generators and the design
below are in place; the generated engines and benchmarks are not written yet.
`benchmarks/` remains the working comparison in the meantime — see
[`docs/benchmarks.md`](../docs/benchmarks.md).

## Why this exists

The two comparisons that exist today do not share a schema:

| | schema | measures |
|---|---|---|
| `benchmarks/` | 1 type, 4 fields, 100 rows | ns/op, B/op, allocs/op |
| `benchmarks/buildbench` | N entities, ~8 types each | generate, RSS, compile, rebuild |

So nothing answers "how fast does a query run against a 200-entity CRUD
schema?", which is where plan-cache pressure, deep connection nesting, large
selection sets and introspection over ~1 600 types would actually show up.

## Intended layout

    compare/
      schema/          one SDL file per entity, shared by both engines
      shared/          the Go structs both engines bind to
      gqlgen/          gqlgen.yml, generated code, generated resolvers
      graphqlgo/       gqlc.yaml, generated code, generated resolvers
      compare_test.go  both engines must return byte-identical JSON
      bench_test.go    the same operations against both

Generated code is produced by `go generate` and git-ignored: gqlgen emits
394 845 lines at 200 entities, roughly 14 MB, which does not belong in the
repository.

## The fairness problem, and how it is solved

The engines model resolvers differently by default, so the obvious setup
measures two different workloads:

- **gqlgen** puts composite fields in the model struct (`Owner *Entity001`)
  and serves them from struct data. Its `ResolverRoot` asks only for
  `Query()` and `Mutation()`.
- **graphql-go** emits models holding only leaf fields and makes every
  composite field a resolver method.

Comparing those directly would pit a struct-pointer read against a resolver
call. gqlgen can be made to match, per field:

    models:
      Entity000:
        fields:
          owner:    { resolver: true }
          children: { resolver: true }

which adds `Entity000() Entity000Resolver` to `ResolverRoot`. Verified at two
entities. Note that gqlgen keeps the composite fields on the model struct even
then, so the two engines' models still differ in size; binding both to the
shared `shared/` package removes that difference as well.

Both engines support that binding: gqlgen through `models: X: model:`, and
gqlc through `Config.Models`.

## What remains

1. Generate `schema/` and `shared/` for a chosen N.
2. Generate each engine's config, including gqlgen's per-field
   `resolver: true` entries.
3. Generate resolver implementations for both, over the shared store. At 200
   entities that is roughly 2 000 methods per engine, so they have to be
   emitted, not written.
4. A correctness test asserting both engines return identical JSON for the
   same queries — the strongest claim this module can make.
5. Benchmarks: shallow connection list, deep nesting, a mutation, and full
   introspection.
