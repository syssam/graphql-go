# Coming from gqlgen

What behaves differently from gqlgen, and so does not show up as a compile
error. Each item here was hit by a real port: a gqlgen server moved onto
graphql-go and `gqlc`, with the old and new servers diffed on one database.

For how to configure the port, `preset: gqlgen` in `gqlc.yaml` reads gqlgen's
directives, tags and list shapes, and `autoBind` and `models` carry over; see
[`cmd/gqlc/README.md`](../cmd/gqlc/README.md).

## HTTP status of a request error

gqlgen answers a document that fails validation with **422**. graphql-go
follows the [GraphQL over HTTP specification](graphql-http-audit.md), where
the status depends on the media type the client accepts:

| Client sends `Accept:` | Parse or validation error | Error with no data, code `INTERNAL_SERVER_ERROR` |
| --- | --- | --- |
| `application/json`, `*/*`, or nothing | **200**; the errors are in the body | 200 |
| `application/graphql-response+json` | **400** | 500 |

A request body that is not a GraphQL request at all is 400 under either media
type, and a batch is always 200. A client that branches on 422 has to read
`errors[].extensions.code` instead (`GRAPHQL_VALIDATION_FAILED`,
`GRAPHQL_PARSE_FAILED`, `BAD_USER_INPUT`: see the `Code*` constants), or ask
for `application/graphql-response+json` and branch on 400.

The engine writes no `status` entry into an error, and its internal error code
is `INTERNAL_SERVER_ERROR`. An `extensions.status`, or a code such as
`INTERNAL_ERROR`, comes from an error presenter in the application
(`WithErrorPresenter`). If the status line and the body disagree, look there
first.

## DataLoaders: no timer

A gqlgen DataLoader usually waits a few milliseconds and dispatches whatever
has queued. `loader.Loader` has no timer: the executor dispatches a batch when
every resolver in the operation is either finished or waiting on a loader, so
one level of the tree is one batch, and a single read waits for nothing. A
ported loader with a 2 ms window made `task(id)` about twice as slow at the
median (4.8 ms against 2.8 ms) until the window was cut. With `loader.New` the
window is gone.

[`tasks/n-plus-one.md`](tasks/n-plus-one.md) has the code and a test that
counts the batches.

## Enums and types that marshal themselves

`graphql.Enum[T]` binds a Go string type, or any comparable type, from a map
of values. No `MarshalGQL` is needed, and gqlc writes the map itself for an
enum it models.

A type that already has gqlgen's `MarshalGQL(io.Writer)` and
`UnmarshalGQL(any) error` binds with `graphql.EnumMarshaler` or
`graphql.ScalarMarshaler`. The check is structural. The type does not need to
import gqlgen's `graphql` package, and if it does, gqlgen stays in your build.
An ORM plugin generating those methods for graphql-go alone can drop that
import. See [`tasks/custom-scalar.md`](tasks/custom-scalar.md).

## A nil slice

A nil Go slice is `[]` at a non-null list position and `null` at a nullable
one. gqlgen writes `[]` for both. A client that treats `null` and `[]`
differently will see a difference at `[T!]` fields.

## Generated code that compiles and is wrong

Two things pass the build and the test suite under both generators. gqlc
reports them:

- **A field bound to something else.** With `autoBind`, a field the ORM type
  gains is bound to it on the next generate, replacing the Resolver method
  that answered it. A generate names each such field in a note, and `-check`
  lists them below the stale files:
  `Task.createdAt: resolver TaskCreatedAt → field CreatedAt`. If the resolver
  computed something different from the column, force it back with
  `createdAt: { resolver: true }` under `fields: Task:` in `gqlc.yaml`, or `@goField(forceResolver: true)` under
  `preset: gqlgen`.
- **A stub never filled in.** A scaffolded method still holds
  `panic("not implemented: ...")` until you write it, and no test that skips
  its field notices. A generate names the stubs left after every run, and
  `gqlc -check -stubs` fails while any remain. That is the line for CI:

  ```sh
  go tool gqlc -config gqlc.yaml -check -stubs
  ```
