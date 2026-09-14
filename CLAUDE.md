# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go vet ./... && go test -race -count=1 ./...   # the standard gate for every change
go test -race -run TestExecNestedLists .       # one test in the root package
go test -short ./codegen                       # skip the slow subprocess compile tests
go test -run xxx -bench . -benchmem .          # microbenchmarks
go test -run xxx -fuzz FuzzExecute -fuzztime 30s .
```

`benchmarks/` is a **separate Go module** (with a `replace` back to the root) because it
depends on gqlgen; `./...` from the root does not reach it:

```sh
cd benchmarks && go test -run '^$' -bench . -benchmem -count=5
cd benchmarks && go generate    # regenerate graph/generated.go via the gqlgen CLI
```

Regenerate the example's bindings after editing its SDL:

```sh
cd examples/basic && go generate    # go run ../../cmd/gqlc -config gqlc.yaml
```

`codegen` tests write a temp module, run `go build`/`go test` in it, and take ~20s; they
honour `-short`.

**`-race` is not optional.** The DataLoader N+1 race was caught 4 times in 40 runs with
`-race` and 0 times in 40 without it: the detector perturbs scheduling enough to hit the
interleaving. Dropping `-race` to save time silently disables the only thing that finds
this bug class. `testing/synctest` does not help here and makes it worse — its
deterministic scheduling hid the same bug in 20 of 20 runs. Use synctest for tests that
would otherwise wait on real time (see `wave_test.go`), not to find races.

Comparing performance between two versions goes through `benchstat`, not by eye:

```sh
go test -count=10 -run '^$' -bench . -benchmem > old.txt   # before
go test -count=10 -run '^$' -bench . -benchmem > new.txt   # after
benchstat old.txt new.txt                                  # golang.org/x/perf/cmd/benchstat
```

Single samples on this codebase have been wrong by 20-77% when the machine was warm.

## Architecture

Schema-first runtime with a code-first binding API. SDL is the contract; Go bindings are
plain function values. Everything expensive happens once, at start-up or first plan
compile — the request path writes JSON straight into a pooled buffer.

**Build time — `NewSchema` (`schema.go`, `source.go`, `registry.go`, and the binding files
`scalar.go`, `enum.go`, `object.go`, `abstract.go`, `input.go`, `directive.go`).**
`gqlparser.LoadSchema` parses the SDL, then `SchemaOption`s (`Object`, `Field`, `Resolve`,
`Input`, `Args`, `Enum`, `Scalar`, `Interface`, `Union`, `Directive`, `Query`/`Mutation`/
`Subscription`) register typed adapters in `registry`, keyed by `(GraphQL type name,
reflect.Type)`: leaf writers, decoders, nil checks, list traversers, `shapeInfo`.
`schemaBuilder.build()` then runs six ordered phases — object shells → input decoders →
abstract types → fields → schema directives → coverage validation. Order matters: shells
must exist before fields reference them, inputs before args are composed. Go-vs-SDL shape
mismatches are reported here as joined errors, never at request time.

**Plan compile (`plan.go`).** A document is parsed and validated once and
cached in an LRU (`docEntry`); each `(operation, @skip/@include variant)` compiles to an
immutable `plan` with fragments flattened, directives constant-folded per variant (up to
`maxCondVars` boolean variables), arguments pre-decoded and response keys pre-serialized.
`selectionSet` carries `byType` for abstract parents plus the scheduling counts the
executor needs.

**Execute (`exec.go`, `exec_object.go`, `internal/jsonw`).** `Executor` owns the plan cache,
the concurrency semaphore, interceptor chains and the limit options. `execState.writeObject`
walks the plan writing into a `jsonw.Writer`; null bubbling rewinds the writer to a recorded
offset rather than building an intermediate value tree. `errNonNull` is the internal signal
for "null reached a non-null position"; `indexedError` carries a list index so error paths
can be reconstructed.

**The pure/resolver split is the core scheduling contract.** `Field`/`FieldArgs` are pure
data access and always run inline with no goroutine and (absent field interceptors) no
context allocation. `Resolve`/`ResolveArgs` may do I/O and are scheduled concurrently under
the bounded semaphore; `Inline()`/`Concurrent()` override per field. `loader.Loader` (`loader/`)
coalesces `Load` calls within one concurrent wave — the executor announces a wave before
launching sibling tasks (`pushWave`), which is what makes DataLoader batching work.

**Codegen (`codegen/`, `cmd/gqlc`).** SDL-only: it never loads Go packages. It emits models,
args structs, a `Resolver` interface and bindings that call the same public constructors as
hand-written code. Each group emits a single `generated.go` holding its args, `Resolver`
interface and bindings — one file per package, since that is the unit the compiler rebuilds.
One SDL group stays flat in `Output`; two or more become subpackages plus a `Resolvers` struct, with models split the same way (`model/<group>/`) so a one-group edit
does not invalidate every other group's compiled package — except when two groups' input
objects reference each other, which would be an import cycle and falls back to one shared
`model` package (`modelGroupsAcyclic`). Generated files are strings run through `go/format` (not Jennifer), and
content-equal files are not rewritten.

`transport/gqlhttp` is the GraphQL-over-HTTP handler; `internal/jsonw` is the only internal
package (it has no dependency on engine types — the plan compiler and executor deliberately
live in the root package so generic constructors can produce engine values directly).

## Conventions

- **Root package may depend only on `gqlparser/v2` and the standard library.** Transports,
  codegen and extensions keep their dependencies in sub-packages.
- **No reflection on the request hot path.** Reflection is allowed at `NewSchema`, in
  `Args[T]`/`Input[T]` decode, and in the one documented composite nested-list traverser
  (which logs `slog.Warn` at start-up). Adding reflection to the write path is a regression.
- Go 1.27 minimum (generic methods, `reflect.TypeFor`).
- Tests live beside the code in `package graphql`; the shared fixture schema and executor
  are in `fixture_test.go` (`newFixtureExecutor`, `run`, `expectData`) — reuse them instead
  of building new schemas per test. `exec_conformance_test.go` is the spec-behaviour suite;
  `api_align_test.go` guards the public binding surface.
- Request-scoped extension state goes in via `OperationContext.GetOrSet`, never
  `Get` then `Set` — `lint/` has an analyzer that catches this at compile time
  (`cd lint && go build -o gqlvet ./cmd/gqlvet`, then `gqlvet ./...` from the repo
  root). It finds the original bug instantly where the test found it 4 times in 40
  runs, and only under `-race` — concurrent sibling resolvers hit their first `Load` together, and
  the check-then-act pair silently gives each one its own copy. `-race` will not catch
  it; the symptom is DataLoader batching intermittently degrading to N+1.
- Package documentation lives in `doc.go`; `graphql.go` holds only the primitive public
  types (`ID`, `Root`, `Omittable`). Moving code between files in a package is free and
  invisible to callers, so keep each file focused enough to guess from its name.
- Comments explain why, not what. English only. No code-narrating comments.
- Commit messages: imperative, lower-case type prefix (`feat:`, `fix:`, `test:`, `refactor:`,
  `docs:`).

## Design documents

`docs/superpowers/specs/2026-09-11-graphql-go-design.md` is the design rationale, but its
**"Phase 1 Deviations" and "Phase 2 Deviations" sections are the authoritative behaviour**
where they disagree with the body (e.g. `Object[User]` not `Object[*User]`; nullable input
positions require a pointer/slice even with a default; complexity/depth/cost are `Executor`
options, not an `ext/complexity` package; `Manifest`/`AutoBind` codegen are not implemented).
Read the deviations before trusting the prose. `docs/superpowers/plans/` holds the phase
implementation plans; `docs/benchmarks.md` holds the gqlgen comparison.

Status: phase 1 complete, phase 2 (codegen) in progress. Not yet built: subscriptions over
WebSocket/SSE, APQ, OpenTelemetry, codegen auto-bind and manifest modes.
