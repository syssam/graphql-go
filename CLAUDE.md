# CLAUDE.md

Schema-first GraphQL engine for Go with a code-first binding API. This file is loaded every
session, so it holds only what applies everywhere. Subsystem detail lives in
`.claude/rules/*.md`, and each file loads only when you open a file in its area:

| Rule | Loads for | Covers |
|---|---|---|
| `schema-build.md` | `schema.go`, binding files, `input.go` | `NewSchema` phases, error ordering, `json:"-"` |
| `plan.md` | `plan*.go`, `limits.go`, `interceptor.go` | plan cache, parse flight, depth/cost guard, cost |
| `executor.md` | `exec*.go`, `wave.go`, `subscription.go`, `internal/jsonw`, `loader/` | response/error limits, timeout, pure/resolver scheduling, `Park`, struct sizes |
| `authz.md` | `authz*.go`, `examples/storefront` | `AuthShape`, requirement directives, instance sites, batching |
| `codegen.md` | `codegen/`, `cmd/gqlc/` | AutoBind, discovery order, enums, the real-schema record |
| `transports.md` | `transport/`, `internal/httpreq`, `internal/gqlwsproto` | shared rules, drain, age/idle, gqlfiber specifics |
| `ext.md` | `ext/`, `fed/`, `relay/`, `stats.go` | otel, runtime metrics, apq, trusted, throttle |
| `verification.md` | `scripts/`, `.github/`, benchmarks, lint config | the full evidence behind "Verification" below |

If you need a subsystem's rules before touching a file in it, read the rule file directly.

## Commands

```sh
go vet ./... && go test -race -count=1 ./...   # root module; the per-change gate
go test -race -run TestExecNestedLists .       # one test
go test -short ./codegen                       # skip the ~20s subprocess compile tests
sh scripts/gate.sh [-short]                    # vet + test EVERY module (there are four)
GATE_REQUIRE_ALL=1 sh scripts/gate.sh          # and fail on a skipped module, as CI does
```

`go test ./...` reaches one module of four: `benchmarks/`, `compare/` and `lint/` are separate
modules. **Run `scripts/gate.sh` before calling a change clean.** `compare/` is generated and
gitignored; the gate skips it unless you run `cd compare && go run gen.go -n 25` first.

Four gates are separate CI jobs and do not run under `go test ./...`:

```sh
go test -run TestPublicAPISurface .                 # pins docs/public-api.txt; -update-api after a deliberate change
go test -run TestAllocationBaseline .               # one-sided alloc gate; skips under -race; -update-allocs to rebase
go install golang.org/x/vuln/cmd/govulncheck@latest && govulncheck ./...   # every module
go test -run "^$" -fuzz FuzzString -fuzztime 30s ./internal/jsonw          # targets: go test -list 'Fuzz.*' per package
```

Lint: `golangci-lint run ./...`, run in each module; the root `.golangci.yml` applies to all
of them, and CI runs it in each -- it did not until a check found `benchmarks/` carrying 18
findings and `compare/` 4, never asked. `.golangci.yml` is the policy and every disabled check
carries its reason. Build `staticcheck`, `golangci-lint` and `govulncheck` with the Go named in
`go.mod`, because an older build fails with `export data version ...` and reports nothing useful.
The repository's own analyzer: `cd lint && go build -o gqlvet ./cmd/gqlvet`, then `gqlvet ./...`
from the root.

Codegen: `cd examples/blog && go generate` after editing its SDL (CI fails on stale output);
`cd benchmarks && go generate` regenerates the gqlgen side. Generation runs through `go tool`
directives in `go.mod`, not `tools.go`.

Performance comparisons go through `benchstat`, with two test binaries run **interleaved**. Use
the `/bench-compare` skill: single samples here have been wrong by 20-77%, and a sequential
before/after once reported a strictly-cheaper change as 50% slower.

## Verification

- **`-race` is not optional.** It caught the DataLoader N+1 race 4 times in 40 runs, and a run
  without it caught it 0 times. `testing/synctest` hides that bug class. Use synctest only to
  replace real waiting (see `wave_test.go`).
- **Break it on purpose.** A test counts only if it fails when the thing it guards is broken
  (`/break-it`). Several guards here are *deliberately redundant*: subscription release
  (`cancelAll` plus the context tree), idle `Stop` plus the `closeIfIdle` re-check, and the
  safelist (`apq.Resolve` refusing query text against a `TrustedStore` plus `trusted.Store.Set`
  being a no-op). Break one half alone and every *end-to-end* test stays green, so a green
  suite does not show that either half is dead code. **The answer is a unit test on the half's
  own contract, not a note saying it cannot be tested** — three of these were carried on
  reasoning until a review drove each one directly: the safelist's no-op `Set`, `cancelAll`'s
  loop (registered cancels the connection context cannot reach), and `closeIfIdle`'s re-check
  (which needs no real timing — hold `mu`, start the callback, re-arm, unlock). The idle `Stop`
  is the one genuine exception and says so in place: without it the re-check still answers
  correctly, so there is no behaviour to assert.
- **Undo a deliberate break with a reverse edit, never `git checkout -- <file>` or
  `git restore`.** Those revert to HEAD and destroy uncommitted work. A hook blocks them.
- **Watch for results that pass while covering less than they appear to.** Sixteen instances
  so far, each described in `verification.md`. The tooling ones: `go test ./...` skipping three
  modules, fuzz discovery that missed `internal/jsonw`, the gate skipping `compare/` in CI,
  lint reaching one module, a `govulncheck` built with an older Go exiting 0 having analyzed
  nothing, `-cover` reporting `internal/httpreq` at 12.8% when it is 89.7%, and `go tool cover`
  reporting 0.0% on an empty function body that is fully tested. The test-shaped ones: a leak
  test passing against a broken path, a benchmark timing dropped events, generated code that
  read fine and did not compile, a pool-cap test that agreed with the constant because its
  input came from the same estimate, a guarded-list test that measured the drain instead of the
  traversal it was written for, and a fix verified only against a consumer schema outside this
  tree. And the two that are about doing the break itself: a break that did not compile, and a
  reverse edit that landed on the wrong occurrence — **check `git diff` after undoing a break,
  not just that the suite is green**. Measure coverage of `internal/` packages with
  `-coverpkg=./internal/X/... ./internal/X/... ./transport/...`.
- A generated-code change is verified by compiling the output. For anything touching
  `NewSchema` against the real consumer schema, run `NewSchema` itself, not only `go build`
  (see `codegen.md`).
- **High coverage is not the signal.** Every file this repository has reviewed line by line was
  already above 90%, and most findings were in lines the tests *did* execute without asserting
  anything about them: a sort whose order nothing checked, a memo whose sharing nothing
  observed, a `[T!]!` fixture that never exercised the nullable shape where the outcome
  differs. Coverage finds untested branches; it says nothing about untested *properties*. The
  method that works is reading each claim a comment makes and breaking it.
- Show evidence: paste the command and its result rather than saying "tests pass".

## Architecture

Nothing expensive happens per request. The expensive work runs once, at start-up or at the
first plan compile, and the request path writes JSON straight into a pooled buffer.

- **Build** (`NewSchema`): gqlparser loads the SDL. `SchemaOption`s register typed adapters in
  `registry`, keyed by `(GraphQL type, reflect.Type)`. `build()` runs six ordered phases, and a
  Go/SDL shape mismatch is a build error, never a request-time error.
- **Plan** (`plan.go`): parse and validate once per query text, in a byte-bounded LRU. Each
  `(operation, @skip/@include variant)` compiles to an immutable `plan`. Abstract expansion is
  memoized into a DAG, so **every walk over a plan must memoize on `*selectionSet`**.
- **Execute** (`exec.go`, `exec_object.go`, `internal/jsonw`): `execState.writeObject` walks
  the plan. Null bubbling rewinds the writer; it never builds a value tree. `Field` is pure and
  inline; `Resolve` is scheduled concurrently under a semaphore; `loader.Loader` batches per
  announced wave.
- **Subscriptions**: every event runs the whole operation chain.
- **Codegen** (`codegen/`, `cmd/gqlc`): SDL only. It loads no Go packages unless `AutoBind`
  asks, and then only export data.
- **Transports**: `gqlhttp`, `gqlsse`, `gqlws`, `gqlecho`, `gqlfiber` all serve every operation
  kind. `internal/httpreq` holds the shared CSRF, body-limit and negotiation rules, and
  `transport/equivalence_test.go` proves all six HTTP handlers agree.
  `internal/gqlwsproto` is the shared graphql-transport-ws core. `transport/drain` drains
  long-lived connections.
- **Authorization** is compiled into the plan (`AuthShape`), not wrapped around it. An
  unguarded field pays one integer compare.
- Extensions: `ext/otel`, `ext/apq`, `ext/trusted`, `ext/throttle`, `fed/`, `relay/`.

**Hot-path structs are pinned to size classes.** `execState` is 64 bytes, `OperationContext`
208 and `planField` 176 (`TestStructSizes`). Before adding a field to any of them, check
`unsafe.Sizeof` and run an interleaved benchstat.

Do not act on a CPU profile alone: `jsonw.(*Writer).overLimit` looks like the hot spot and
measurably is not (`executor.md`).

The root package is ~32 files on purpose, because Go scopes encapsulation to the package; see
`docs/root-package.md` before proposing a split.

## Conventions

- **The root package imports only `gqlparser/v2` and the standard library.** The other
  dependencies in `go.mod` (yaml, coder/websocket, echo v5, fiber v3, OpenTelemetry) belong to
  sub-packages. The Fiber websocket module is `gofiber/contrib/v3/websocket`. The path without
  `/v3/` is the Fiber v2 module and will not build.
- **No reflection on the request hot path.** Reflection is allowed at `NewSchema`, in
  `Args[T]`/`Input[T]` decode, and in the one documented nested-list traverser.
- Go 1.27 minimum (generic methods, `reflect.TypeFor`).
- Root tests are `package graphql`. Reuse the fixture in `fixture_test.go`
  (`newFixtureExecutor`, `run`, `expectData`) instead of building a new schema per test.
  `exec_conformance_test.go` is the spec suite, and `api_align_test.go` guards the binding
  surface.
- Request-scoped extension state goes in via `OperationContext.GetOrSet`, never `Get` then
  `Set`: the pair silently degrades DataLoader batching to N+1, and `-race` cannot see it.
  `gqlvet` can.
- A nil Go slice is written as `null`. For `[T!]!`, return `make([]T, 0, n)` when the result
  is empty.
- Package docs go in `doc.go`. `graphql.go` holds only `ID`, `Root`, `Omittable` and the
  scalar `Writer`. Keep each
  file focused enough that its name tells you what is in it.
- Comments explain why, not what. English only. No code-narrating comments.
- Commits: imperative, with a lower-case type prefix (`feat:`, `fix:`, `test:`, `refactor:`,
  `docs:`).

## Design documents and status

- `docs/superpowers/specs/2026-09-11-graphql-go-design.md` is the design rationale. **Its
  "Phase N Deviations" sections override the body.** Read them before trusting the prose.
  Plans are in `docs/superpowers/plans/`.
- `docs/api-stability.md`: the public API contract, including why a gqlparser v3 forces a major
  version here. `docs/operations.md`: deployment limits and sizing, ending with what has not
  been measured. `docs/benchmarks.md`: the comparison with gqlgen and the per-transport costs.
  `docs/module-layout.md`: why this stays one module until publication.
- Examples: `quickstart` (smallest), `blog` (layering, codegen; `echo`/`fiber` serve it),
  `storefront` (authorization and production wiring), `federation`, `relaynode`.
- **Status:** phases 1-4 are merged. **Not built:** `ext/authz` (only a batched `Guard` is
  left; `@policy` and `@authenticated` are already one option call each). **Declined:**
  `@defer`/`@stream`, which `introspection.go` strips on purpose, so adding them reverses a
  decision. **Not measured:** anything in production, latency percentiles, and behaviour under
  a cgroup memory limit (the last two need Linux).
- A 5 503-type ent-derived ERP schema is the **test corpus**, not the requirements: it found
  twelve bugs and one mis-sized constant, every one of which would break for any consumer, and
  the features it motivated are all opt-in and off by default. `codegen.md` has the trajectory
  (33 924 `NewSchema` errors to **zero**, over eleven passes, with nothing hand-written but the
  ten scalar bindings). **Do not relax the nullable-`bool` input rule to make a number go
  down**, and do not read its schema's shape as a requirement -- it is gqlgen's shape, which is
  what this library exists to leave.

Keep this file and the rules honest and short. A statement that contradicts another part is
worse than silence. When a fact is only relevant to one subsystem, put it in that subsystem's
rule file, not here.

When compacting, preserve: the list of modified files, the exact test and gate commands run
with their results, and any deliberate break still in place that has not been reversed yet.
