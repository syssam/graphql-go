# Decision needed: one module or five

This is a question for the project owner, written once so it does not arrive
three times from three directions. Two implementation sessions have each
deferred it and recorded the same reasoning; the numbers below are measured,
not estimated.

**The question.** Should `codegen`, `ext/otel`, and the Echo and Fiber
transports become their own Go modules, or stay in the root module?

## What it costs today

Everything a consumer's `go.mod` resolves, whether or not they import it:

| | Modules in the graph |
|---|---:|
| Phase 2 (engine + codegen) | 9 |
| Phase 3 (+ subscriptions, WebSocket, SSE) | 10 |
| **Phase 4 (+ OpenTelemetry, auto-bind)** | **35** |

Phase 4 added 25 modules. `coder/websocket`, in phase 3, added one. The jump is
almost entirely two features:

- **`ext/otel`** pulls the OpenTelemetry API *and* SDK. The API is small and
  genuinely needed at runtime; the SDK is test-only here but is still a direct
  require, and brings `go-logr`, `google/uuid`, `xxhash` and `x/sys`.
- **`codegen`'s auto-bind** pulls `golang.org/x/tools`, and with it `x/mod`,
  `x/sync`, `x/net`, `x/telemetry`, `goldmark` and the rest of the gopls
  dependency tail.

None of these link into a binary that does not import them. The cost is
resolution, download and audit surface, not runtime size — with one exception:

| `cmd/gqlc` | before auto-bind | after |
|---|---:|---:|
| Binary | 5.4 MB | **9.7 MB** |
| Peak RSS, 200-entity schema | 44 MB | **82 MB** |

`codegen` imports `x/tools/go/packages` unconditionally, so `gqlc` links it
whether or not anyone uses `AutoBind`. That moved a published figure: peak
generation memory was **105x** less than gqlgen and is now **56x**. Still
decisive, but it moved because of a feature most users will never enable, and
for a project whose stated premise is avoiding gqlgen's build weight that is
the wrong direction.

## What splitting costs

One real thing, and it is not obvious:

**The stated gate stops covering the split packages.** `go vet ./... && go
test -race -count=1 ./...` is this repo's gate for every change, and it reaches
only the current module. `benchmarks/`, `compare/` and `lint/` are already
outside it, and during a correctness audit this session they were nearly missed
— they build and pass, but only because someone remembered to check them by
hand. Splitting four more packages means four more things the gate silently
stops testing.

That is fixable with a script or CI matrix that walks every `go.mod`, but it
has to be written and it has to be the thing people actually run. Until then,
splitting trades a dependency problem for a coverage problem.

Secondary costs: users of `codegen` or `ext/otel` need a second `go get`;
`cmd/gqlc` has to move into the codegen module or the root keeps `x/tools`
through it; and `examples/basic`'s `//go:generate go run ../../cmd/gqlc` path
changes. Import paths for library users do **not** change.

## The options

**A. Stay one module.** Simplest, gate keeps working, dependency graph keeps
growing. Defensible while unpublished — nobody is resolving this module yet.

**B. Split all four at once** — `codegen` (with `cmd/gqlc`), `ext/otel`,
`gqlecho`, `gqlfiber`. Recovers roughly 25 modules from the default graph and
~40 MB of `gqlc`'s memory. Requires the multi-module gate script first.

**C. Split at publication, not now.** Record the intent, keep developing in one
module, do it as part of the first tagged release when the gate script and CI
exist anyway.

## Recommendation

**C, with B's scope.** The costs are real but every one of them is paid at
resolution time by consumers, and there are no consumers yet. Splitting now
buys nothing today and weakens the gate during the two weeks when a
subscription-protocol refactor and two new transports are landing — which is
exactly when coverage matters most.

What should happen now is the cheap half: write the multi-module gate script,
so that `benchmarks/`, `compare/` and `lint/` stop being tested by memory, and
so the split is a mechanical change when the time comes rather than a change
that also has to invent its own safety net.

If the answer is B instead, the four should go together rather than in two
passes, so that `gqlecho` and `gqlfiber` are not created in the root module and
moved out a month later.

## What is not in question

The OpenTelemetry *API* (`otel`, `otel/trace`, `otel/metric`) stays wherever
`ext/otel` lives; it is small and it is what instrumentation genuinely needs.
The argument is about the SDK and `x/tools`, which are test- and build-time
only.
