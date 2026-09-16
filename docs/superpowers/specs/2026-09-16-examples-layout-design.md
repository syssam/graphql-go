# examples/ layout

Date: 2026-09-16
Status: approved, not yet implemented

## Problem

`examples/` has three directories and three distinct problems.

**`examples/basic` names things wrongly.** The package called `schema` is not the
schema — it is the resolver and data layer (`resolvers.go`, `store.go`,
`broker.go`). The thing that actually builds the schema is `graph/schema.go`,
which is generated. A reader opening `schema/` to find the SDL finds a store.

**The SDL exists twice, and nothing says which one to edit.** The duplication
itself is not a defect and cannot be removed: `codegen/emit.go` copies every
source SDL into `<output>/schema/<basename>` so that the generated
`graph/schema.go` has something for its `//go:embed schema/*.graphql` to find.

```go
// codegen/emit.go
for _, src := range b.sources {
	base := filepath.Base(src.Name)
	files[filepath.ToSlash(filepath.Join("schema", base))] = []byte(src.Input)
}
```

The defect is that `examples/basic/schema/schema.graphql` (the source) and
`examples/basic/graph/schema/schema.graphql` (the generated copy) are named
alike, sit at comparable depths, and carry no marker. Editing the second is
silently undone by the next `go generate`.

Pointing `gqlc.yaml` at the copy so that only one file exists was tried against
the real generator and does work — input and output resolve to the same path,
the content is equal, and `writeGo`'s equality check skips the write. It is
still rejected: it puts the one file a human is supposed to edit inside the tree
headed `DO NOT EDIT`. The fix is to make the roles obvious instead, by lifting
the source to `blog/schema.graphql` and leaving the copy inside `graph/`.

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
│   ├── schema.graphql              THE SOURCE SDL — edit this one
│   ├── graph/                      generated; DO NOT EDIT
│   │   ├── schema/schema.graphql   generator's copy, for //go:embed
│   │   ├── generated.go
│   │   ├── schema.go
│   │   └── model/models.go
│   ├── internal/
│   │   ├── domain/                 User, Post — does not import graphql-go
│   │   ├── repository/             in-memory store + broker
│   │   ├── app/                    use cases; the rules CreatePost enforces
│   │   └── transport/graphql/      implements graph.Resolver; maps domain ↔ model;
│   │                               owns the author DataLoader
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

`internal/app` holds the rules and nothing else. A prototype of this layout was
built and measured before this spec was finalised, and the first version of
`app` — the one this section originally described — came out at **seven methods,
all seven of them a single forwarding call to the repository**. That is the
cargo-cult outcome, and it was fixed by moving rules down rather than by deleting
the layer: `CreatePost`'s "an unknown author is an error" and "a blank title is
an error" now live in `app`, and the repository stores what it is given.

The honest figure after that change is **five of seven methods still forwarding**.
That is the real shape of a use-case layer over an in-memory store and the README
must not pretend otherwise. What it buys is that a second caller — an import job,
an admin tool — cannot reach the repository and skip the validation.

The `Post.author` DataLoader does **not** belong here. It exists to batch the N+1
that GraphQL's field-at-a-time resolution creates; nothing outside the GraphQL
adapter would ever construct one, so it lives in `internal/transport/graphql`.
This corrects an earlier draft of this spec that placed it in `app`.

If implementation finds `app` sliding back toward all-forwarding, drop the layer
and let `internal/transport/graphql` call the repository directly. Three layers
that each do something beat four where one is scenery.

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

**The mapping costs a lookup on exactly the field the DataLoader exists for.**
`Post.author` is a resolver field in the SDL, so the generated `model.Post` has
no `AuthorID`, while `domain.Post` does. `PostAuthor` therefore has to fetch the
post back out of the store by id to recover an author id the domain object was
already holding, before the loader can batch the user fetch. Binding
`Object[domain.Post]` directly would hand the resolver the `AuthorID` for free.

This is not a regression — `examples/basic` already pays it today via
`r.store.authorID(p.ID)` — but it is the clearest single illustration of what
the isolation costs, and the README should use it as such rather than hide it.

## Already verified against the real generator

This layout was not reasoned about and written down. A throwaway prototype of it
was built at `examples/verify`, generated, compiled, tested under `-race`, and
then deleted. What that established, and what it changed in this spec:

| Claim | Result |
|---|---|
| `gqlc` copies the source SDL into `<output>/schema/` | True — it is why the second copy exists. Rewrote the problem statement. |
| A single self-referential SDL path works | True, and rejected anyway: it files the editable SDL under `DO NOT EDIT`. |
| The generated `Resolver` can be implemented from a sub-package over mapped domain types | True. Compiles and serves interfaces, unions, enums, `Omittable` input and a subscription. |
| `//go:generate` in `blog.go` is reached by `go generate ./...` | True, and generation is idempotent (output hashes unchanged on a second run) — which is what CI's staleness check needs. |
| `examples/echo` can import `blog.NewSchema` | True. |
| `examples/echo` cannot import `blog/internal/...` | True, enforced by the compiler: `use of internal package ... not allowed`. |
| `app` as originally specified | **False start.** 7 of 7 methods were forwarding calls. Rules moved down from the repository; now 5 of 7. |
| The DataLoader belongs in `app` | **Wrong.** Moved to `internal/transport/graphql`. |

The prototype's tests — interface `node`, union `search`, loader-backed
`author`, both `createNote` validation rules, and subscription delivery plus
subscriber release on cancel — all passed under `-race`.

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
