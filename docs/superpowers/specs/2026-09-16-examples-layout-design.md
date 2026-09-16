# examples/ layout

Date: 2026-09-16
Status: approved, not yet implemented

## Problem

`examples/` has three directories and three distinct problems.

**`examples/basic` names things wrongly.** The package called `schema` is not the
schema — it is the resolver and data layer (`resolvers.go`, `store.go`,
`broker.go`). The thing that actually builds the schema is `graph/schema.go`,
which is generated. A reader opening `schema/` to find the SDL finds a store.

**The SDL exists twice.** `examples/basic/schema/schema.graphql` is the source
`gqlc.yaml` reads; `examples/basic/graph/schema/schema.graphql` is the copy the
generator writes and `graph/schema.go` embeds. Nothing marks which is which, and
editing the wrong one is silently undone by the next `go generate`.

**`examples/echo` and `examples/fiber` are the same program twice.**
`resolvers.go` (162 lines) and `schema.graphql` are byte-identical between them.
Only `main.go` differs, and only in framework wiring.

There is no README anywhere under `examples/`, so nothing says which example a
newcomer should read first.

## What is worth keeping

The `echo` and `fiber` `main.go` files are the most valuable prose in
`examples/`. They record why `e.Any`/`app.All` rather than per-method routes,
why Fiber's `BodyLimit` is set alongside `gqlfiber`'s own limit, why
`ShutdownWithTimeout` cannot take a bare context, and why the default origin
check is left strict. None of that is duplicated, and none of it is obvious.
The duplication is entirely in the note-board app sitting beside them.

`examples/basic/schema/{schema_test.go,subscription_test.go}` (265 lines, 12
tests) cover interfaces, unions, `Omittable` PATCH semantics, non-null bubbling,
introspection, subscription delivery, subscriber release on cancel, and
backpressure. They are the safety net for this whole change and must survive it.

## Design

```
examples/
├── README.md                       which example teaches what
├── quickstart/                     hand-written bindings, no codegen, no layers
│   ├── main.go
│   ├── notes.go
│   └── schema.graphql
├── blog/
│   ├── blog.go                     package blog — NewSchema() is the only public surface
│   ├── blog_test.go
│   ├── gqlc.yaml
│   ├── graph/                      generated; DO NOT EDIT
│   │   ├── schema/schema.graphql   the single SDL copy
│   │   ├── generated.go
│   │   ├── schema.go
│   │   └── model/models.go
│   ├── internal/
│   │   ├── domain/                 User, Post — does not import graphql-go
│   │   ├── repository/             in-memory store + broker
│   │   ├── app/                    use cases; the author DataLoader
│   │   └── transport/graphql/      implements graph.Resolver; maps domain ↔ model
│   └── cmd/server/main.go          net/http: gqlhttp + gqlsse + gqlws
├── echo/main.go                    imports blog; nothing else
└── fiber/main.go                   imports blog; nothing else
```

### quickstart is the note board, de-duplicated

`examples/echo/resolvers.go` already describes itself as deliberately skipping
codegen "to stay copyable as a single small package". That is precisely the role
a quickstart plays, so `quickstart/` is that file and its SDL moved up one level
and kept once, not a newly invented fourth example. The `store`/`broker`/binding
code moves to `notes.go`; `main.go` keeps the `net/http` wiring.

It stays `package main` with hand-written bindings. It is the answer to "show me
the smallest thing that runs", and it is the contrast case for the layering
below: `Object[Note]` binds the domain type directly, with no mapping layer.

### blog is the layered service

`internal/domain` holds `User` and `Post` as plain Go types that do not import
`graphql-go`. `internal/repository` stores those. `internal/transport/graphql`
implements `graph.Resolver` and maps `domain.User` to the generated `model.User`
at the boundary.

`internal/app` earns its place or it does not get created. What it holds is the
work that is neither storage nor protocol: the `Post.author` DataLoader, which is
request-scoped batching over the repository and belongs to neither side of it,
and `CreatePost`'s rule that an unknown author is an error. If the implementation
finds itself writing a method whose body is one call to the repository and
nothing else, that method belongs in `internal/transport/graphql` calling the
repository directly, and the layer should be dropped rather than padded.

`graph/` stays outside `internal/` although only `internal/transport/graphql`
imports it. Generated output is something a reader of this example is meant to
open and compare against `gqlc.yaml`, and hiding it would also complicate the
generator's output path for no benefit.

Today `store.go` stores `*model.User` — the generated GraphQL model — as its
database record, so a change to the SDL changes the database's type. The split
is the same isolation a protobuf-shaped service keeps between its domain
entities and its wire messages.

This is a demonstration, not a requirement the library imposes. `graphql-go`
binds `Object[domain.User]` directly and `quickstart` does exactly that. The
README must say so in as many words, or the example misrepresents the library as
needing ceremony it does not need.

### One public surface, so echo and fiber stay where readers look

Go forbids `examples/echo` from importing `examples/blog/internal/...`. Rather
than drop `internal/` — which is half of what makes the layout legible — `blog`
exports exactly one function:

```go
// package blog
func NewSchema(opts ...graphql.SchemaOption) (*graphql.Schema, error)
```

`cmd/server`, `examples/echo` and `examples/fiber` each call it and wire their
own transport. Three frameworks serving one schema demonstrates the
transport-independence the README currently only asserts.

### This reverses an earlier decision, deliberately

`docs/superpowers/plans/2026-09-15-echo-fiber-transports.md` specified that each
transport example be "self-contained: its own SDL and resolvers, copyable as a
starting point without tracing shared files". The duplication was intended.

It is not worth its cost. Two byte-identical 162-line files drift the moment one
is edited, and the copyable-starting-point role is served better by
`quickstart/`, which is smaller and has no framework in it at all. A reader
copying `examples/echo` wants the routing and shutdown wiring; they are going to
substitute their own schema regardless of whether one ships in the same
directory.

### Tests

`blog_test.go` lives in `package blog`, which can reach `internal/...` because it
is inside `examples/blog/`. To let it construct a store and a schema over that
same store, `blog.go` carries an unexported second constructor:

```go
func NewSchema(opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	return newSchema(repository.NewStore(), opts...)
}

func newSchema(repo *repository.Store, opts ...graphql.SchemaOption) (*graphql.Schema, error)
```

`TestSubscriberIsReleasedOnCancel` reads `store.created.subscribers()` today, an
unexported field of an unexported type. After the split that field belongs to
`package repository`, so `repository.Store` gains an exported `Subscribers() int`
documented as existing for the leak test.

The alternative — moving that test into `package repository` — was rejected. The
test's value is that it drives `Executor.Subscribe` and proves executor teardown
reaches the broker. A repository-level test calling `PostsCreated` directly would
assert a tighter invariant and stop covering the one that actually breaks.

All 12 tests keep their queries and their expected responses byte for byte. What
may change is how a test reaches a fixture — `store.created.subscribers()`
becomes `store.Subscribers()`, `NewSchema(NewStore())` becomes
`newSchema(repository.NewStore())`. A test whose *expectation* has to be edited
to pass is a signal that the refactor changed behaviour, and is grounds to stop
rather than to update the expectation.

## Deliberately not included

- **No `Dockerfile`, no `Makefile`.** Both would restate `go generate`, and
  nothing in CI would build either, so both would rot untested.
- **No `config/` package.** An in-memory blog has nothing to configure. The
  `-addr` flag belongs in each `main.go`, where it is being read.

## Out of scope

Files under `docs/superpowers/plans/` and `docs/superpowers/specs/` dated before
today are records of what was built then. They are not updated to match this
layout; editing them to agree with the present would destroy their value as
history.

## Files changed outside examples/

| File | Change |
|---|---|
| `.github/workflows/ci.yml` | `cd examples/basic && go generate ./...` → `examples/blog`, and the staleness message with it |
| `README.md` | two `examples/basic` links → `examples/blog` |
| `CONTRIBUTING.md` | the "Generated code" section's path |
| `cmd/gqlc/README.md` | "`examples/basic/gqlc.yaml` writes `examples/basic/graph`, and `examples/basic/schema` implements `graph.Resolver`" |
| `CLAUDE.md` | the `cd examples/basic && go generate` command |
| `docs/module-layout.md` | one mention of `examples/basic` |

## Risks

**Renaming `examples/basic` to `examples/blog` breaks external links.** Accepted:
the directory is not a published import path anyone depends on, and `basic` is
the wrong name for the layered example now that `quickstart` exists.

**The domain/model mapping adds boilerplate** to `internal/transport/graphql`
that does not exist today. Accepted as the point of the example, on condition
that the README names the direct-binding alternative.

## Verification

1. `go build ./examples/...`
2. `cd examples/blog && go generate` leaves the tree clean (`git diff --quiet`),
   which is what CI checks.
3. `sh scripts/gate.sh` — every module, `-race`, not just the root.
4. All 12 moved tests pass with their assertions unchanged.
5. `go run ./examples/echo`, `./examples/fiber` and `./examples/blog/cmd/server`
   each answer the same query on `/graphql` and the same subscription on
   `/graphql/stream`.

Step 5 is the one that catches what the others cannot: three mains that compile
against one schema still prove nothing until each has actually served it.
