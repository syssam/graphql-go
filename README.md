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

A fuller example with interfaces, unions, enums, custom scalars, input objects,
`Omittable` PATCH semantics, a schema directive, a DataLoader for `Post.author`
and a subscription fed by the `createPost` mutation lives in
[`examples/basic`](examples/basic).

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
| `relay.Node` / `relay.Bind[T]` | Relay global object identification and cursor connections in `graphql-go/relay`. `ToGlobalID`/`FromGlobalID`, `IDField`, `Pagination`, `FromSlice`/`FromPage`. The SDL still declares the types. |
| `loader.New` / `loader.NewMapped` | Per-request batch+cache (Facebook DataLoader) in `graphql-go/loader`. `Load` coalesces concurrent Resolve fields in one execution wave, driven by `graphql.WaveCoordinator`. `NewMapped` reports failure per key, so one bad id does not null its siblings. |

Resolvers can read their context with `graphql.FieldFrom`, `graphql.PathFrom`
and `graphql.SelectionFrom`; `graphql.OperationFrom` exposes the operation,
variables, complexity, depth and cost. `SetExtension` writes response-level
metadata (tracing ids, rate-limit windows).

Executor options follow the gRPC style: `WithMaxConcurrency`, `WithPlanCache`,
`WithErrorPresenter`, `WithRecover`, typed interceptors, plus production
limits `WithMaxComplexity`, `WithMaxDepth` and `WithQueryCost` (Shopify-style
`first`/`last` multipliers, `Connections` to price a Relay connection by its
requested page size, and optional `extensions.cost`; `Actual` adds
`actualQueryCost`, summed from the fields really resolved rather than from
assumed list sizes). Schema options:
`DisableIntrospection`.

Empty `Args[T]()` / `Input[T](name)` derive every field (`graphql` tag, else
`json` tag, else `AuthorID` → `authorId`). Passing any `InputField` switches
that struct to fully explicit setters; the two modes do not mix.

## Status

Phases 1 and 2 of the
[design](docs/superpowers/specs/2026-09-11-graphql-go-design.md) are complete:
`cmd/gqlc` emits models, args, a `Resolver` interface and bindings from SDL,
splitting into per-group packages when more than one group is present, and
[`examples/basic`](examples/basic) uses the generated package.

Phase 3 is under way. Subscriptions execute, and both streaming transports are
built: `transport/gqlsse` over Server-Sent Events and `transport/gqlws` over
`graphql-transport-ws`, with `connection_init`/`ack`, `ping`/`pong`, an
`OnConnect` hook whose context parents every operation on the connection, an
init timeout and a per-connection operation cap. Both serve queries and
mutations too -- one `next` then `complete` -- so a client needs only one
endpoint. Automatic persisted queries are in `ext/apq`, opt-in on either HTTP transport
with `WithPersistedQueries(apq.NewCache(1000))`. OpenTelemetry traces and
metrics are in `ext/otel`: `graphql.NewExecutor(s, otel.New()...)`. Cost-based rate
limiting is in `ext/throttle`: a bucket of points per caller refilled at a fixed
rate, quoted before the query runs and charged what it really cost afterwards,
reporting `extensions.cost.throttleStatus` the way Shopify's Admin API does.

Phase 4 is under way. `QueryCost.Actual` reports `actualQueryCost` alongside
the requested one, summed from the fields really resolved rather than from
assumed list sizes. `codegen.Config.Manifest` binds GraphQL types and fields
to Go types outright instead of inferring them, still without loading any Go
type information, which is the mode an external generator such as an ORM
wants, and `AutoBind` discovers the same bindings from named packages,
loading only their export data rather than a whole module. Still to come: APQ
over WebSocket.

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
