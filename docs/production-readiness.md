# Production readiness: graphql-go with velox

Written 2026-09-26 for the question "could a company the size of Google, Uber,
Meta, Anthropic or SAP run this?" The answer is written from evidence in this
repository and velox's, and says where evidence is missing.

## Verdict

**Not yet, and the reasons are not the code's quality.** The engineering is at
the level those companies expect in the places that decide correctness and
cost -- compiled authorization, bounded query cost, SQL-level row filtering,
drain on shutdown, differential tests against gqlgen and graphql-js. What is
missing is what only time, releases and users produce:

| Blocker | Evidence | What clears it |
|---|---|---|
| No release | graphql-go has no tag; consumers pin commits. velox is v0.3.0, pre-1.0, with breaking changes allowed without a deprecation cycle. | Tag graphql-go; ship velox v1.0 with the COMPATIBILITY.md guarantees in force. |
| No production hours | `docs/operations.md`: "This engine has not served a real request outside a benchmark." | Run one non-critical internal service behind the company's existing gateway, shadowing a known-good one, and compare. |
| One maintainer | Both repositories. | A second owner for each, and a security contact that is not the author. |
| No external security review | Authorization, input coercion and the transports are reviewed and fuzzed in-repo only. | An independent review of `authz*`, `coerce.go` and `transport/`. |

A large company adopts a dependency like this by vendoring it, running it
behind its own gateway, and owning a fork until upstream has the track record.
Everything below is what makes that fork cheap.

## What already meets the bar

| Concern | Where |
|---|---|
| Authorization decided before execution, per field and per row; masking (per row with `RedactRow`: a customer sees their own email), null, zero, deny | `authz*.go`, `examples/storefront`, `examples/veloxfx/authz.go` |
| A withheld field costs no database query | `SelectedField.Withheld` + velox `contrib/graphqlgo` |
| Row ownership as a SQL filter, not load-then-drop | velox read interceptors; `examples/veloxfx` `ownOrders` |
| Query depth, complexity and cost limits, priced per connection page | `limits.go` (`Connections: true`) |
| Persisted and trusted documents (Meta and Shopify style) | `ext/apq`, `ext/trusted` |
| Rate limiting by cost | `ext/throttle` |
| Tracing and metrics | `ext/otel`, velox `contrib/otelvelox` |
| Federation (Apollo router), including gqlc-generated subgraphs | `fed/` (`SubgraphFS`), gqlc `federation: true`; velox `graphqlgo.Entities` answers a router's batch in one query per type |
| Graceful drain of HTTP, SSE and WebSocket | `transport/drain` |
| Introspection off in production | `DisableIntrospection()` |
| N+1 prevention without DataLoaders for edges | velox field collection, now engine-neutral |
| PostgreSQL and MySQL | velox CI runs its dialect matrix; the combined example runs its suite on PostgreSQL in CI |
| Supply chain | `govulncheck` in CI for every module; root package depends only on gqlparser |

## What this round found by running real-world shapes

Each of these was wrong, silently, until a test shaped like production traffic
ran:

- A computed field over an edge answered **0** once field collection narrowed
  the edge's columns (velox, fixed; `Map(...).Loads` declares it now).
- A **foreign-key violation on PostgreSQL** was reported as a duplicate: the
  example matched SQLite's message text (fixed; the CI job above found it).
- `database/sql`'s default pool spent **9% of CPU** reconnecting on PostgreSQL
  under load (fixed; `configurePool`, -37% per request).
- A WebSocket token expiring during a write dropped the socket without the
  **1001** that makes clients re-authenticate (fixed; graphql-go gqlws).
- Connection cost priced `first: 1` as fifty rows per level, which refuses
  legitimate pages or forces budgets that protect nothing (use
  `Connections: true`).
- Directives on velox edges were **dropped**, so an edge "guarded" by
  `@requiresScopes` was served unguarded (velox, fixed).
- A **gqlc-generated schema could not be a federation subgraph**: `fed` took
  one SDL string, and gqlc could not parse `@key`. Now `federation: true`.
- gqlc **never deleted an SDL copy** whose source was removed, so a deleted
  file's types stayed in the served schema -- found when a removed
  federation link kept the subgraph carrying two (fixed).

That list is the argument for the adoption path above: every item passed a
green unit suite and failed the first time something shaped like real traffic
touched it.

## A large schema without federation

`docs/large-schemas.md` measures a 300-entity monolith through velox, gqlc
and graphql-go. Start-up (114 ms) and introspection (4.3 MB, under 70 ms) are
fine; the edit loop is the cost, three minutes on 4 cores for an entity edit.
It found that graphql-go's generic binding functions were half of that
rebuild's CPU (fixed, -35% on the group packages), that the documented depth
limit locked IDEs and client generators out of introspection (fixed), and
that gqlparser's default `MaxIntrospectionDepth` rule is exponential in
fragment spreads -- a 1.1 KB document cost two seconds of validation, with or
without introspection enabled (replaced here; gqlgen uses the same rule).

## What is still unmeasured or open

- Latency percentiles and throughput of the combined stack on production
  hardware; the figures here come from a 4-core container with PostgreSQL on
  the same box.
- Behaviour across a PostgreSQL failover, and under connection-pool
  exhaustion at the database's own limit.
- A validator such as `NonNegative()` cannot check `AddX`; database `CHECK`
  constraints are the fix and velox does not emit them.
- gqlgen is still in velox's root module graph, for the gqlgen side of its
  GraphQL support; a server generated with `graphqlgen.WithoutGQLGen()` links
  none of it (`examples/veloxfx`: 0 gqlgen packages).
