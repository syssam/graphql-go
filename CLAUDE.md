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

**`go test ./...` reaches one module and this repository has four.** `benchmarks/`,
`compare/` and `lint/` are outside the root module, so the gate above does not test them:

```sh
sh scripts/gate.sh            # vet and test every module
sh scripts/gate.sh -short     # skip the slow subprocess and load tests
```

Use it before claiming a change is clean. During one audit the three outside modules were
very nearly missed, and they had a root API change in them at the time.

**Watch for measurements that succeed while covering less than they look.** A red build is
easy; a command that used to be complete, quietly stopped being, and still prints success is
not. Four instances in one session, all found by asking what a passing result would look
like if the thing under test were broken:

- `go test ./...` printed `ok` for every package it knew about, and had silently stopped
  reaching three modules as they were added.
- A subscription leak test passed against a deliberately broken release path, because a
  second redundant path still freed everything. It only failed when both were broken.
- A fan-out benchmark reported 45ns per subscriber because it timed a `publish` that drops
  into a full buffer. Counting receipts put it at 4us — the events it was "measuring" had
  reached nobody.
- Generated code that read correctly and did not compile, twice: a method taking the
  generated args struct (import cycle) and `ID!` bound to a `string` field. Both were found
  by compiling the output, neither by reading it.

The habit that catches these is breaking the thing on purpose and requiring the test to
fail. If it still passes, the test was agreeing with the code rather than checking it.

`benchmarks/` is a separate module (with a `replace` back to the root) because it
depends on gqlgen:

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

`transport/gqlws/load_test.go` opens 150 concurrent subscriptions and requires both the
source registrations and the goroutines back afterwards; it honours `-short`.
`BenchmarkSubscriptionFanout` measures a broadcast reaching every subscriber by counting
receipts, because `publish` drops rather than blocks and timing it alone reports a fan-out
to 128 clients at 45ns each when the honest figure is 4us. **Releasing an operation is
doubly redundant** — the operation context derives from the connection context, and
`cancelAll` also calls each stored cancel — so breaking either leaves every test green and
only breaking both leaks. Do not read a green suite as evidence that one of them is dead.

**Subscriptions (`subscription.go`).** `Subscribe`/`SubscribeArgs` bind a subscription root
field to a function returning `<-chan R`; `Executor.Subscribe` plans the operation, opens
the stream and returns `<-chan *Response`, one per event, closed when the source closes or
ctx is cancelled. Each event builds its own `OperationContext` and runs the whole operation
chain, so a DataLoader cache cannot outlive the event that filled it. The per-event writer
substitutes the root field's executor with one that yields the event and then calls the
ordinary `writeObject`, which is why null bubbling and error paths need no special case —
and why field interceptors do not see that one field. A subscription root field bound with
`Field` or `Resolve` is rejected at `NewSchema`.

**Codegen (`codegen/`, `cmd/gqlc`).** SDL-only: it never loads Go packages. It emits models,
args structs, a `Resolver` interface and bindings that call the same public constructors as
hand-written code. Each group emits a single `generated.go` holding its args, `Resolver`
interface and bindings — one file per package, since that is the unit the compiler rebuilds.
`Config.Manifest` binds types and fields outright instead of inferring them, which is the
mode an external generator wants; it still loads no Go type information, so a method binding
declares its own shape (`Context`, `Error`). Type bindings are folded into `cfg.Models` in
`newBuilder`, so model references, imports and `mapped` keep working unchanged and only
field kinds and group overrides are read from the manifest afterwards. **A method's
arguments are spread into the call, not passed as the generated args struct** — passing the
struct makes the bound type's package import the generated one, which already imports it for
the model, and that is an import cycle. A type in the manifest gets no inference at all: an
unlisted field is a resolver.

`Config.AutoBind` discovers bindings from named packages instead of being told them, and
produces a `Manifest` — so discovery is the only new behaviour and everything downstream is
the manifest path. **Only export data is loaded** (`NeedName | NeedTypes | NeedImports |
NeedDeps`): adding `NeedSyntax` or `NeedTypesInfo` would parse every file of every package,
which is the cost this project exists to avoid, so treat either as a regression. Matching is
json tag, then case-insensitive name, then a method whose shape the generator can call;
anything else falls through to `Resolver`, because a resolver method can always be written
where a bad guess is a compile error in code the user did not write. A Go type over the same
basic kind gets a conversion (`graphql.ID(v.ID)`), since an ORM storing an id as a `string`
still answers `ID!` and refusing would send the commonest field in any schema through a
resolver.

One SDL group stays flat in `Output`; two or more become subpackages plus a `Resolvers` struct, with models split the same way (`model/<group>/`) so a one-group edit
does not invalidate every other group's compiled package — except when two groups' input
objects reference each other, which would be an import cycle and falls back to one shared
`model` package (`modelGroupsAcyclic`). Generated files are strings run through `go/format` (not Jennifer), and
content-equal files are not rewritten.

**Transports.** `transport/gqlhttp` is the GraphQL-over-HTTP handler; `transport/gqlsse`
streams over Server-Sent Events (distinct connections mode); `transport/gqlws` speaks
`graphql-transport-ws` over `coder/websocket`. All three serve every operation kind — a
query or mutation is one `next` then `complete` — so a client needs one endpoint. The two
HTTP transports parse requests through `internal/httpreq`, so a request one rejects as
forgeable or oversized is rejected by the other; drift there is visible to clients.

**Actual query cost (`limits.go`).** `QueryCost.Actual` sums the weight of every field
really resolved, alongside the requested cost computed from assumed list sizes; the two
together say whether `DefaultListSize` is near reality. Weights are resolved onto
`planField.costWeight` at plan compile, so the write path adds an integer rather than
looking up a coordinate, and the weight is zero unless the feature is on.

**Adding a field to `execState` or `OperationContext` is a hot-path change.** Both are
allocated per request and both sit exactly on a size-class boundary. An `atomic.Int64`
counter on `execState` measured +3.2% B/op with the feature disabled; as an `atomic.Int32`
packed beside `cancelled` it measures zero. Check with `unsafe.Sizeof` and `benchstat`
before growing either, and interleave the runs — a non-interleaved comparison on this
machine reported a 13.8% regression that vanished at n=18.

`ext/otel` instruments an executor with OpenTelemetry: `graphql.NewExecutor(s, otel.New()...)`.
One span per request, started before parsing so a parse failure still produces one and
renamed once the operation is known. **Metrics are recorded at the operation layer and at
the request layer only when the operation chain never ran** — recording at both double-counts
every request, which the metric test caught. Field spans are opt-in and cost more than they
look: a field interceptor routes every field through the type-erased path, pure ones
included.

`ext/apq` is automatic persisted queries, opt-in through `WithPersistedQueries` on either
HTTP transport. **Resolution happens during parsing, not at execution**: a request carrying
only a hash has no query text, so the "mutations are not allowed over GET" guard would have
nothing to inspect and would wave a persisted mutation through. Registration verifies
`sha256(query) == hash` — storing whatever text arrived would let one client choose what
every later client's hash executes. `httpreq` takes a `queryOptional` flag so that with APQ
off the missing-query errors are byte-identical to before.

In `gqlws`, **writes use the connection context, never the operation's**: coder/websocket
tears down the whole connection when a write context is cancelled mid-frame, so writing a
`next` under the operation context would let one client's unsubscribe drop every other
subscription on that connection. The init timeout likewise closes the connection from a
timer rather than bounding the read, because a read aborted by its own context leaves no
way to send the 4408 close frame.

`internal/jsonw` is the output writer and has no dependency on engine types — the plan
compiler and executor deliberately live in the root package so generic constructors can
produce engine values directly.

## Conventions

- **Root package may depend only on `gqlparser/v2` and the standard library.** Transports,
  codegen and extensions keep their dependencies in sub-packages. `go.mod` therefore also
  carries `yaml.v3` (for `cmd/gqlc`), `coder/websocket` (for `transport/gqlws`) and the
  OpenTelemetry API and SDK (for `ext/otel`, the SDK only in its tests); the rule is about
  what the root package imports, not about module purity. `ext/otel` is the obvious
  candidate to split into its own module at publication time, so that the SDK leaves every
  consumer's module graph.
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
**"Phase N Deviations" sections are the authoritative behaviour** where they disagree with
the body (e.g. `Object[User]` not `Object[*User]`; nullable input positions require a
pointer/slice even with a default; complexity/depth/cost are `Executor` options, not an
`ext/complexity` package; `Manifest` and `AutoBind` landed in phase 4 rather than phase 2,
and the phase 4 deviations describe how). Read the deviations before trusting the prose.
`docs/superpowers/plans/` holds the phase implementation plans; `docs/benchmarks.md` holds
the gqlgen comparison and `docs/module-layout.md` the decision to stay one module until
publication.

Status: phases 1-4 complete and merged to `main` — engine, both codegen binding modes,
subscriptions, three transports, DataLoader, APQ, limits with actual cost accounting,
OpenTelemetry, and the `lint/` analyzer. Not built: APQ over WebSocket, which belongs in the
`graphql-transport-ws` state machine. Not measured, both needing Linux: latency percentiles,
which this machine's ~522us clock granularity makes impossible, and behaviour under a
cgroup memory limit.

Keep this section honest. It said "subscription load testing not yet built" while the
architecture section above described `transport/gqlws/load_test.go` in detail, and called
`Manifest`/`AutoBind` unimplemented while documenting both — a file that contradicts itself
is worse than one that says nothing, because a reader trusts the half that happens to agree
with them.
