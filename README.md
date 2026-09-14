# graphql-go

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
- **Specification complete.** Null bubbling, list element errors, fragments,
  `@skip`/`@include`, variable coercion, interfaces and unions, introspection
  (including `specifiedByURL`, `isOneOf`, deprecated arguments) and the GraphQL
  over HTTP protocol.
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

A fuller example with interfaces, unions, enums, custom scalars, input objects,
`Omittable` PATCH semantics, a schema directive and a DataLoader for
`Post.author` lives in [`examples/basic`](examples/basic).

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
| `loader.New` | Per-request batch+cache (Facebook DataLoader) in `graphql-go/loader`. `Load` coalesces concurrent Resolve fields in one execution wave, driven by `graphql.WaveCoordinator`. |

Resolvers can read their context with `graphql.FieldFrom`, `graphql.PathFrom`
and `graphql.SelectionFrom`; `graphql.OperationFrom` exposes the operation,
variables, complexity, depth and cost. `SetExtension` writes response-level
metadata (tracing ids, rate-limit windows).

Executor options follow the gRPC style: `WithMaxConcurrency`, `WithPlanCache`,
`WithErrorPresenter`, `WithRecover`, typed interceptors, plus production
limits `WithMaxComplexity`, `WithMaxDepth` and `WithQueryCost` (Shopify-style
`first`/`last` multipliers and optional `extensions.cost`). Schema options:
`DisableIntrospection`.

Empty `Args[T]()` / `Input[T](name)` derive every field (`graphql` tag, else
`json` tag, else `AuthorID` → `authorId`). Passing any `InputField` switches
that struct to fully explicit setters; the two modes do not mix.

## Status

Phase 1 of the [design](docs/superpowers/specs/2026-09-11-graphql-go-design.md)
is complete. Phase 2 codegen is started: `cmd/gqlc` emits models, args, a
`Resolver` interface and bindings from SDL, splitting into per-group
packages when more than one group is present. [`examples/basic`](examples/basic)
uses the generated package. Upcoming work adds auto-bind, then subscriptions
over WebSocket and SSE, automatic persisted queries and OpenTelemetry.

## Development

```sh
go vet ./... && go test -race -count=1 ./...
go test -run xxx -bench . -benchmem .
go test -run xxx -fuzz FuzzExecute -fuzztime 30s .
cd benchmarks && go test -run '^$' -bench . -benchmem -count=5
```

Runtime comparison with gqlgen (same SDL, same 100 users) lives in
[`docs/benchmarks.md`](docs/benchmarks.md). Bindings can be generated from
SDL with [`cmd/gqlc`](cmd/gqlc).
