# Security

## Reporting a vulnerability

Report privately through [GitHub's security advisory
form](https://github.com/syssam/graphql-go/security/advisories/new) rather than
as a public issue.

Please include what an attacker can do, not only what is wrong: a request that
reproduces it, the schema shape it needs, and whether it requires a valid
operation. A minimal SDL plus a query is ideal.

## Scope

This is a GraphQL server engine, so the interesting classes are:

- **Resource exhaustion from a single request.** Depth, breadth, alias
  multiplication, fragment expansion. `WithMaxComplexity`, `WithMaxDepth` and
  `WithQueryCost` exist to bound these and are **off by default** — a server
  that enables none of them can be made to do arbitrary work by an
  unauthenticated client. That is a documented default, not a vulnerability;
  a way around the limits when they *are* set is a vulnerability.
- **Cross-request data leaking**, through the plan cache, a pooled buffer
  returned twice, or request-scoped state shared between operations. Pooled
  response buffers are the sharp edge: `Response.Release` returns one to the
  pool, and reading a response after releasing it is a use-after-free that can
  surface as another request's data.
- **Anything reachable without a valid operation**: parser or validator
  crashes, unbounded allocation before the cost limits are applied.
- **Persisted queries.** Registration verifies `sha256(query) == hash`; a way
  to store text under a hash that does not match it would let one client
  choose what every later client's hash executes.
- **Transport-level forgery.** `gqlhttp` and `gqlsse` reject requests a browser
  could send cross-origin without a preflight; `gqlws` verifies the Origin
  header by default. A bypass is in scope, as is a way to reach a mutation
  over GET.

## Not in scope

- Resolvers doing something unsafe with their arguments. The engine decodes and
  validates against the schema; what a resolver then does with a value is the
  application's responsibility.
- Denial of service through sheer request volume, which is a deployment
  concern.
- The default-off limits described above, when left off.
- `ref/`, which holds unmodified copies of other projects for reference and is
  neither built nor shipped.

## Supported versions

Pre-release: only `main` is supported. Once tagged, this section will name the
supported minor versions.
