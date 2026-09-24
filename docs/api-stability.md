# API stability

What this library will and will not change, and the one coupling that decides
its major version for it.

Nothing is tagged yet, so nothing below is a promise in force. It is the shape
the promise will take, written now because the decisions are cheaper to make
before a version number exists than after.

## What is covered

Everything in [`public-api.txt`](public-api.txt): 413 exported declarations
across 15 importable packages, regenerated and compared by
`TestPublicAPISurface` on every run. A change to any of them is a failing test
and a visible diff in the commit that makes it, reported by symbol:

```
--- FAIL: TestPublicAPISurface
    the public API has changed.
      + graphql: func AccidentallyExported()
```

Not covered, and deliberately: `internal/`, `examples/`, `ref/`, and the
`benchmarks`, `compare` and `lint` modules, which have their own `go.mod` and
are not part of this one. Unexported struct fields are stripped from the
snapshot, so an internal layout change is not an API change; `TestStructSizes`
is what guards the sizes that matter.

## The coupling that decides the major version

**`gqlparser/v2`'s AST is part of this library's public API.** Five
declarations hand it to callers:

| Declaration | Exposes |
|---|---|
| `Schema.AST()` | `*ast.Schema` |
| `Executor.OperationKind()` | `ast.Operation` |
| `OperationContext.Operation`, `.Doc` | `*ast.OperationDefinition`, `*ast.QueryDocument` |
| `FieldContext.Field`, `.Object` | `*ast.FieldDefinition`, `*ast.Definition` |
| `AuthSite.Field`, `.Object` | the same two |

The consequence is not obvious and is worth stating plainly: **if gqlparser
releases a v3, this library cannot adopt it without a major version of its
own**, because `*ast.Schema` from v2 and from v3 are different types and every
signature above would change.

That is accepted rather than overlooked. The alternative is a parallel set of
types wrapping the AST, which is a large surface to maintain, loses information
the moment gqlparser adds a node, and gives an Authorizer or a schema directive
less to work with than the parser already knows. gqlgen makes the same trade
for the same reason. The decision is recorded here so that a v3 of gqlparser is
a planned major release rather than a surprise.

## Three option idioms, and why they differ

They look inconsistent in the snapshot, so: they are, and two of the three are
closed sets that only this package can produce.

| Type | Form | Can a consumer write one? |
|---|---|---|
| `SchemaOption`, `FieldOption`, `InputFieldOption`, `AbstractOption` | interface with an unexported method | No. The unexported method closes them. |
| `FieldSchedule` | `func(*fieldSpec)` over an unexported type | No. `fieldSpec` cannot be named outside the package, so only `Inline()` and `Concurrent()` produce one. |
| `ExecutorOption` | `func(*Executor)` | Syntactically yes; usefully no. Every field of `Executor` is unexported, so an option written elsewhere can read and write nothing. |

`ExecutorOption` is the one that reads as extensible and is not. It stays a
function type because that is what every `WithX` returns and changing it would
touch every option in the library and every extension package; the limitation
is documented rather than designed away.

## Pre-release changes already made

- **`FieldOpt` removed.** It was a deprecated alias for `FieldSchedule`, kept
  so existing code kept compiling. A deprecation alias exists to protect
  released users; there are none, and carrying it past a v1 tag would mean
  carrying it forever. Removed now, when it costs nothing.

## Still expected to change

Named so that a consumer can avoid them or accept the risk knowingly.

- **`AuthShape`, `AuthSite`, `Decision`, `Outcome`, `ObjectCheck`.** The
  authorization spine is the newest surface here and the only one with a single
  consumer. `AuthSite.ListElement` was added during this work because a policy
  could not otherwise choose between `Drop` and `Deny`; that it was missing at
  all says the shape has not been used widely enough to be settled.
- **`WaveCoordinator`.** It exists for extension authors who schedule their own
  batched work, and `loader` is the only thing that has ever driven it. The
  protocol — `OnReady`, `Park`, `Unpark` — is right for that one consumer and
  untested against a second.
- **`QueryCost`.** Adding a field to it is source-compatible; changing what an
  existing one means is not, and `Connections` and `Actual` are both recent
  enough that their defaults may move.
- **`codegen.Config`.** A generator's configuration grows with the generator.

Everything else — the binding constructors, the transports, the interceptor
chain, `Error`, `Request`, `Response`, `loader`, `relay`, `fed` — is what the
examples are built on and is not expected to move before a tag.

## What a version number will mean

Ordinary Go semantics, with one clarification each way:

- **A patch or minor release may add** an exported symbol, a field to a struct
  a consumer does not construct with positional literals, or a new option.
  `TestPublicAPISurface` makes each of those a deliberate diff.
- **A change in behaviour with no change in signature is still a breaking
  change** where a consumer could depend on it. The defaults in
  [`operations.md`](operations.md) — 64 MiB responses, 1000 errors, a 16 MiB
  plan cache, `GOMAXPROCS*4` resolver slots — are part of the contract, not
  implementation detail.
