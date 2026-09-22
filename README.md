# graphql-go

[![CI](https://github.com/syssam/graphql-go/actions/workflows/ci.yml/badge.svg)](https://github.com/syssam/graphql-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/syssam/graphql-go.svg)](https://pkg.go.dev/github.com/syssam/graphql-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/syssam/graphql-go)](https://goreportcard.com/report/github.com/syssam/graphql-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A schema-first GraphQL runtime for Go with a code-first, type-safe binding API.

`graphql-go` is built for large schemas: hundreds of types, thousands of
fields, generated or hand-written. It keeps the build fast because bindings
are ordinary function values assembled at start-up, and it keeps requests fast
because every operation is compiled once into an immutable plan and streamed
straight into a pooled JSON buffer.

## Why

- **Small generated surface.** Bindings are plain calls to generic
  constructors (`Object`, `Field`, `Resolve`, `Input`, `Enum`, `Scalar`).
  Nothing is emitted per field beyond a one-line closure, so a 200-entity
  schema generates in under a second using 44 MB, where gqlgen needs 31 s and
  4.6 GB. See [`docs/benchmarks.md`](docs/benchmarks.md) for what this does
  and does not buy at compile time.
- **Typed output path.** Go types are matched to SDL types once in
  `NewSchema`. Resolvers, scalar writers and explicit `InputField` setters
  are then ordinary function values. `Args[T]()` / `Input[T](name)` may
  reflect once per input value; that is decode only.
- **Specification conformance.** Null bubbling, list element errors, fragments,
  `@skip`/`@include`, variable and argument coercion, `@oneOf` input objects
  from literals and variables alike, interfaces and unions, introspection
  (including `specifiedByURL`, `isOneOf`, deprecated arguments and directive
  deprecation) and the GraphQL over HTTP protocol. The gaps that remain are all
  in the parser rather than the engine; each is tracked with a reproduction and
  an upstream patch in [`docs/spec-conformance.md`](docs/spec-conformance.md).
- **Production behaviour by default.** Bounded resolver concurrency, panic
  recovery, error masking, gRPC-style interceptors, schema directives, CSRF
  prevention, DataLoader batching, and GitHub/Shopify-style complexity, depth
  and query-cost limits.

## Install

```sh
go get github.com/syssam/graphql-go
```

Go 1.27 or newer.

## Quick start

```go
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
)

type User struct {
	ID   graphql.ID
	Name string
}

type userArgs struct{ ID graphql.ID }

func main() {
	users := map[graphql.ID]*User{"1": {ID: "1", Name: "Ada"}}

	s, err := graphql.NewSchema(graphql.SDL(`
		type User { id: ID! name: String! }
		type Query { user(id: ID!): User }
	`),
		graphql.Args[userArgs](),
		graphql.Object[User]("User",
			graphql.Field("id", func(u *User) graphql.ID { return u.ID }),
			graphql.Field("name", func(u *User) string { return u.Name }),
		),
		graphql.Object[graphql.Root]("Query",
			graphql.ResolveArgs("user", func(ctx context.Context, _ graphql.Root, a userArgs) (*User, error) {
				return users[a.ID], nil
			}),
		),
	)
	if err != nil {
		log.Fatal(err)
	}

	http.Handle("/graphql", gqlhttp.New(graphql.NewExecutor(s)))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

```sh
curl -s localhost:8080/graphql -H 'content-type: application/json' \
  -d '{"query":"{ user(id: \"1\") { id name } }"}'
# {"data":{"user":{"id":"1","name":"Ada"}}}
```

## Subscriptions

`Subscribe` binds a subscription root field to a channel. Each value received
becomes one response, with the field's sub-selection executed against it:

```go
graphql.Subscription(
	graphql.Subscribe("postCreated", func(ctx context.Context) (<-chan *Post, error) {
		return blog.PostsCreated(ctx), nil
	}),
)
```

Serve them over either streaming transport; both also serve queries and
mutations, so a client needs only one endpoint:

```go
mux.Handle("/graphql", gqlhttp.New(exec))
mux.Handle("/graphql/stream", gqlsse.New(exec))   // Server-Sent Events
mux.Handle("/graphql/ws", gqlws.New(exec))        // graphql-transport-ws
```

```sh
curl -N localhost:8080/graphql/stream -H 'content-type: application/json'   -H 'accept: text/event-stream'   -d '{"query":"subscription { postCreated { title } }"}'
# event: next
# data: {"data":{"postCreated":{"title":"Live"}}}
```

Closing the connection unsubscribes: the source channel's context is cancelled,
so a broker can drop the subscriber and stop producing.

On shutdown, the server's own `Shutdown` drains ordinary requests but not these:
it neither closes nor waits for WebSockets, and an SSE stream never ends by
itself, so `Shutdown` waits out its whole timeout. A `drain.Drain` handed to both
streaming handlers winds them down — subscriptions end (WebSocket close 1001, or
the SSE response ends without `complete`, so clients reconnect), queries and
mutations already running over a WebSocket finish, and new WebSocket connections
and new SSE subscriptions get 503:

```go
d := drain.New()
mux.Handle("/graphql/stream", gqlsse.New(exec, gqlsse.WithDrain(d)))
mux.Handle("/graphql/ws", gqlws.New(exec, gqlws.WithDrain(d)))

// on SIGTERM: both together, under one deadline
var wg sync.WaitGroup
wg.Go(func() { _ = d.Shutdown(ctx) })
_ = srv.Shutdown(ctx)
wg.Wait()
```

`gqlecho.SSE`/`WS` take the same options, and `gqlfiber.WithDrain` does the same
for Fiber. Past the deadline `d.Shutdown` cuts what is left and returns.

Behind a load balancer, `gqlws.WithMaxConnectionAge(age, grace)` retires WebSocket connections
after `age` (±10%, spread out so they do not all reconnect at once) the same way, so clients reconnect to wherever the balancer now
sends them; `WithMaxConnectionIdle` closes one with nothing in flight, and `gqlsse.WithMaxStreamAge`
ends SSE subscription streams. `gqlfiber` has the same options; all are off by default.

A fuller example with interfaces, unions, enums, custom scalars, input objects,
`Omittable` PATCH semantics, a schema directive, a DataLoader for `Post.author`
and a subscription fed by the `createPost` mutation lives in
[`examples/blog`](examples/blog).
[`examples/quickstart`](examples/quickstart) is a much smaller note board —
hand-written bindings, no codegen.
[`examples/storefront`](examples/storefront) is the one to read before
deploying anything: an order API where four principals see different parts of
the same graph, every position declares what it requires, and the server
carries the limits, persisted queries, tracing and shutdown drain the other
examples leave out. [`examples/README.md`](examples/README.md) compares them.

## The development loop

If you know gRPC you already know the shape: write the contract, generate from
it, implement the interface it hands you, wire it to a transport.

| gRPC | graphql-go |
|---|---|
| write `service.proto` | write `schema.graphql` |
| `protoc --go_out=.` | `go tool gqlc -config gqlc.yaml` |
| implement `XxxServer` | implement the generated `Resolver` |
| `grpc.NewServer()` | `graph.NewSchema(r)`, then a transport |

[`docs/workflow.md`](docs/workflow.md) walks it through, including what `gqlc`
does not do -- it never loads Go packages, which is why a 200-entity schema
generates in under a second -- and the DataLoader step that turns a resolver
field from an N+1 into one query per wave.

## Concepts

| Constructor | Purpose |
|---|---|
| `Object[E](name, fields...)` | Bind a GraphQL object type to Go struct `E`. Field functions receive `*E`. |
| `Field` / `FieldArgs` | Pure data access; runs inline, never on its own goroutine. |
| `Resolve` / `ResolveArgs` | May perform I/O; scheduled concurrently under a bounded semaphore. |
| `Args[A]()` / `Input[T](name)` | Decoder from struct fields (`graphql` / `json` tags, or `AuthorID` → `authorId`). Explicit `InputField` stays the zero-reflect path. |
| `InputField`, `OmittableField` | Hand-written setters when a name or type needs an override. |
| `Enum`, `Scalar` | Leaf types; several Go types may back one GraphQL type. |
| `Interface`, `Union`, `TypeResolver` | Abstract types resolved from the dynamic Go type or an explicit function. |
| `Directive` / `DirectiveArgs[A]` | Schema-directive middleware on `FIELD_DEFINITION` and `OBJECT`. |
| `Query` / `Mutation` / `Subscription` | Bind the schema's root types without repeating their names. |
| `Subscribe` / `SubscribeArgs` | Bind a subscription root field to a `<-chan R` source. `Executor.Subscribe` yields one response per event. |
| `fed.Subgraph` | Apollo Federation subgraph in `graphql-go/fed`: `_service`, `_entities`, the federation directives and an `_Entity` union derived from the `@key` types. No engine change; the author's SDL is never rewritten. |
| `relay.Node` / `relay.Bind[T]` | Relay global object identification and cursor connections in `graphql-go/relay`. `ToGlobalID`/`FromGlobalID`, `IDField`, `Pagination`, `FromSlice`/`FromPage`. The SDL still declares the types. |
| `loader.New` / `loader.NewMapped` | Per-request batch+cache (Facebook DataLoader) in `graphql-go/loader`. `Load` coalesces concurrent Resolve fields in one execution wave, driven by `graphql.WaveCoordinator`. `NewMapped` reports failure per key, so one bad id does not null its siblings. |

Resolvers can read their context with `graphql.FieldFrom`, `graphql.PathFrom`
and `graphql.SelectionFrom`; `graphql.OperationFrom` exposes the operation,
variables, complexity, depth and cost. `SetExtension` writes response-level
metadata (tracing ids, rate-limit windows).

Executor options follow the gRPC style: `WithMaxConcurrency`, `WithPlanCache`,
`WithPlanCacheBytes` (bound the cache by query text, 16 MiB by default),
`WithErrorPresenter`, `WithRecover`, typed interceptors, `WithFieldObserver` (watch every
field, pure ones included, without the type-erased path a field interceptor forces), plus production
limits `WithMaxResponseBytes` (null data and one `RESPONSE_TOO_LARGE` error once a
response's data passes the limit, 64 MiB by default; execution stops early rather than
writing the rest), `WithMaxErrors` (at most 1000 errors a response by default, then one
`ERROR_LIMIT_EXCEEDED` notice), `WithOperationTimeout` (a deadline per query, mutation or subscription
event, reported as `OPERATION_TIMEOUT` rather than the caller's `REQUEST_CANCELLED`; off
by default), `WithMaxComplexity`, `WithMaxDepth` and `WithQueryCost` (Shopify-style
`first`/`last` multipliers, `Connections` to price a Relay connection by its
requested page size, and optional `extensions.cost`; `Actual` adds
`actualQueryCost`, summed from the fields really resolved rather than from
assumed list sizes). Schema options:
`DisableIntrospection`.

Empty `Args[T]()` / `Input[T](name)` derive every field (`graphql` tag, else
`json` tag, else `AuthorID` → `authorId`). Passing any `InputField` switches
that struct to fully explicit setters; the two modes do not mix.

## Status

Phases 1-4 of the
[design](docs/superpowers/specs/2026-09-11-graphql-go-design.md) are complete.

**Engine.** Schema-first with a code-first binding API: SDL is the contract and
Go bindings are plain function values, checked against it at `NewSchema` rather
than at request time. A document is parsed and validated once and cached in an
LRU bounded by entry count and by query text; concurrent misses for one query
share a single parse. Each `(operation, @skip/@include variant)` compiles to an
immutable plan with fragments flattened, directives constant-folded, arguments
pre-decoded and response keys pre-serialized, and abstract types expanded
through a memo so the plan stays a DAG rather than a tree. The request path
writes JSON straight into a pooled buffer, with no reflection on it at all.

**Code generation.** `cmd/gqlc` emits models, args structs, a `Resolver`
interface and bindings from SDL alone -- it never loads Go packages -- splitting
into per-group packages when more than one group is present.
`codegen.Config.Manifest` binds GraphQL types and fields to Go types outright
instead of inferring them, which is the mode an external generator such as an
ORM wants, and `AutoBind` discovers the same bindings from named packages,
loading only their export data rather than a whole module.
[`examples/blog`](examples/blog) uses the generated package.

**Transports.** `gqlhttp`, `gqlsse` (Server-Sent Events), `gqlws`
(`graphql-transport-ws`), and `gqlecho` and `gqlfiber` for Echo v5 and Fiber v3.
All five serve every operation kind, so a client needs one endpoint, and they
share one set of CSRF, body-limit and content-negotiation rules that
`transport/equivalence_test.go` proves by driving real requests through all six
HTTP-carrying handlers. `transport/drain` winds down what `http.Server.Shutdown`
cannot: an SSE stream is a request that never ends and a WebSocket is hijacked,
which `Shutdown` neither closes nor waits for. Connection age and idle limits
mirror grpc's `keepalive.ServerParameters`, all off by default.

**Authorization.** Declared in the SDL and compiled into the plan, not wrapped
around it: a field that declares nothing costs one integer compare and no
allocation. `@requiresScopes` is built in, `RequirementDirective` and
`MarkerDirective` add a schema's own spelling (Apollo's `@policy` and
`@authenticated` are one call each), `@authorizeInput` makes an argument's
contents a site of its own, and `@authorizeObject` decides individual rows
during execution, batched per list. An `Authorizer` receives what the operation
touches and records an outcome per site -- allow, deny, null, zero, redact or
drop -- and `RequireAuthCoverage` fails the build for a field that declares
neither a requirement nor `@public`. [`examples/storefront`](examples/storefront)
wires all of it.

**Extensions.** `ext/apq` for automatic persisted queries, opt-in on every HTTP
transport and on `gqlws` and `gqlfiber`; `ext/trusted` turns the same wiring
into a safelist that runs only documents a build step registered and refuses
query text whatever hash accompanies it, which is what Relay's
`--persist-output` and Apollo's manifest are for; `ext/otel` for traces and
metrics, including DataLoader batch spans (`otel.Batch("user", loadUsers)`) and
runtime instruments registered after the executor and drain exist
(`otel.ObserveExecutor`, `otel.ObserveDrain`, each once per meter); and
`ext/throttle` for cost-based rate limiting, a bucket of points per caller
quoted before the query runs and charged what it really cost, reported as
`extensions.cost.throttleStatus` the way Shopify's Admin API does.

**Limits**, all off by default except the last two: query depth, complexity and
cost (`QueryCost.Actual` reports what really resolved alongside what was assumed
from list sizes, which is how you tell whether `DefaultListSize` is set near
reality), an operation timeout that bounds work rather than latency, a response
size cap (64 MiB), and an error-list cap (1000).

**Also:** `relay/` for global ids, `Node` and cursor connections, shown end to
end in [`examples/relaynode`](examples/relaynode); `fed/` for
Apollo Federation subgraphs, with [`examples/federation`](examples/federation)
showing two of them and the fetch a router performs across them; and `lint/`,
an analyzer for a bug class `-race`
cannot find -- a check-then-act pair on `OperationContext` that silently
degrades DataLoader batching to N+1.

Not built: `ext/authz`, and what is left of it is smaller than a package --
`@requiresScopes` is core, `@policy` is one `RequirementDirective` call and
`@authenticated` one `MarkerDirective`, leaving only a batched `Guard`.
`@defer`/`@stream` is not merely unbuilt: the prelude's `@defer` is stripped so
that the validator and introspection agree the server says no.

## API stability

Nothing is tagged yet. [`docs/api-stability.md`](docs/api-stability.md) records
what the promise will cover, which parts are still expected to move, and the
one coupling that decides this library's major version for it: `gqlparser/v2`'s
AST is part of the public API, so a gqlparser v3 means a major release here.
`TestPublicAPISurface` pins all 413 exported declarations against
[`docs/public-api.txt`](docs/public-api.txt), so an API change is a failing
test rather than a code review someone has to catch.

## Running it in production

[`docs/operations.md`](docs/operations.md) is the deployment page: which limits
to turn on before the engine faces anything untrusted (four are off by
default), how to shut down streams the HTTP server cannot, what sizes a
replica, which metric to alarm on, and the limits to design around -- including
the ones this engine will not do, and the figures nobody has measured yet.

## Development

```sh
sh scripts/gate.sh                 # vet and test every module, not just the root
go vet ./... && go test -race -count=1 ./...
go test -run xxx -bench . -benchmem .
go test -run xxx -fuzz FuzzExecute -fuzztime 30s .
cd benchmarks && go test -run '^$' -bench . -benchmem -count=5
```

Measured performance and specification conformance are summarised in
[`docs/performance.md`](docs/performance.md), which links the detail: query
execution and memory in [`compare/`](compare), build cost in
[`docs/benchmarks.md`](docs/benchmarks.md), and the official GraphQL over HTTP
audit in [`docs/graphql-http-audit.md`](docs/graphql-http-audit.md) — 0 errors,
all 13 MUST requirements met. Bindings can be generated from
SDL with [`cmd/gqlc`](cmd/gqlc).

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request, and
[SECURITY.md](SECURITY.md) to report a vulnerability. Licensed under the
[MIT License](LICENSE).
