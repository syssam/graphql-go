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
  schema compiles in seconds, not minutes.
- **No reflection on the hot path.** Go types are matched to SDL types once in
  `NewSchema`; after that, resolvers, scalar writers and input decoders are
  typed function values.
- **Specification complete.** Null bubbling, list element errors, fragments,
  `@skip`/`@include`, variable coercion, interfaces and unions, introspection
  (including `specifiedByURL`, `isOneOf`, deprecated arguments) and the GraphQL
  over HTTP protocol.
- **Production behaviour by default.** Bounded resolver concurrency, panic
  recovery, error masking hooks, request/operation/field interceptors, schema
  directives, CSRF prevention and body limits in the HTTP transport.

## Install

```sh
go get github.com/syssam/graphql-go
```

Go 1.24 or newer.

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
		graphql.Args[userArgs](graphql.InputField("id", func(a *userArgs, v graphql.ID) { a.ID = v })),
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
`Omittable` PATCH semantics and a schema directive lives in
[`examples/basic`](examples/basic).

## Concepts

| Constructor | Purpose |
|---|---|
| `Object[E](name, fields...)` | Bind a GraphQL object type to Go struct `E`. Field functions receive `*E`. |
| `Field` / `FieldArgs` | Pure data access; runs inline, never on its own goroutine. |
| `Resolve` / `ResolveArgs` | May perform I/O; scheduled concurrently under a bounded semaphore. |
| `Args[A](fields...)` | Decoder for an argument struct, looked up by Go type. |
| `Input[T]`, `InputField`, `OmittableField` | Input objects; `Omittable` distinguishes absent from `null`. |
| `Enum`, `Scalar` | Leaf types; several Go types may back one GraphQL type. |
| `Interface`, `Union`, `TypeResolver` | Abstract types resolved from the dynamic Go type or an explicit function. |
| `Directive[A]` | Schema-directive middleware applied to field definitions. |

Resolvers can read their context with `graphql.FieldFrom`, `graphql.PathFrom`
and `graphql.SelectionFrom`; `graphql.OperationFrom` exposes the operation,
variables and plan complexity to interceptors.

Executor options: `WithMaxConcurrency`, `WithPlanCache`, `WithErrorPresenter`,
`WithRecover`, `WithInterceptors`. Schema options: `DisableIntrospection`.

## Status

Phase 1 of the [design](docs/superpowers/specs/2026-09-11-graphql-go-design.md)
is complete: the runtime, binding API, plan compiler, executor, introspection
and `transport/gqlhttp`. Upcoming phases add code generation (`cmd/gqlc`),
subscriptions over WebSocket and SSE, automatic persisted queries, complexity
limits and OpenTelemetry.

## Development

```sh
go vet ./... && go test -race -count=1 ./...
go test -run xxx -bench . -benchmem .
go test -run xxx -fuzz FuzzExecute -fuzztime 30s .
```
