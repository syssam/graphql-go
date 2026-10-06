# Documentation

## Learn it

There is no tutorial here yet. The path that exists is the examples, in this
order, and they are meant to be read as source; the task pages below it each
do one job from start to finish.

| Start here | For |
| --- | --- |
| [`examples/quickstart`](../examples/quickstart) | One SDL file, hand-written bindings, no codegen. The smallest thing that runs. |
| [README "Concepts"](../README.md#concepts) | Every constructor in one table: `Object`, `Field` vs `Resolve`, `Args`/`Input`, `Enum`, `Scalar`, `Interface`/`Union`, `Directive`, `Subscribe`. |
| [`examples/blog`](../examples/blog) | Layering, generated bindings, a DataLoader, subscriptions. |
| [`examples/storefront`](../examples/storefront) | Authorization, limits, and the server wiring a deployment needs. |
| [`examples/README.md`](../examples/README.md) | What each of the seven examples is for, including `federation` and `relaynode`. |

**The API reference is godoc**: `go doc github.com/syssam/graphql-go`, or
pkg.go.dev once published. [`doc.go`](../doc.go) is the package overview and is
the single best page to read before writing any binding.

## Decide whether to use it

| Document | Answers |
| --- | --- |
| [`spec-conformance.md`](spec-conformance.md) | Which parts of the specification are met, which four gaps are in the parser rather than the engine, and how this engine compares with graphql-js, Apollo Server, graphql-yoga and gqlgen when they are actually run. |
| [`graphql-http-audit.md`](graphql-http-audit.md) | The GraphQL-over-HTTP specification's own audit suite against `gqlhttp`. |
| [`benchmarks.md`](benchmarks.md) | The comparison with gqlgen, including per-transport costs and what was retracted. |
| [`performance.md`](performance.md) | Latency percentiles, subscriptions at scale, schema build cost, and a "what is not measured" section that is the point of the document. |
| [`api-stability.md`](api-stability.md) | What is covered by the compatibility promise, and why a gqlparser v3 forces a major version here. |

## Run it

| Document | Answers |
| --- | --- |
| [`operations.md`](operations.md) | Deployment limits, sizing, what to watch, behaviour in a container, and what has not been measured. |

## Why it is built this way

| Document | Answers |
| --- | --- |
| [`root-package.md`](root-package.md) | Why the root is one package of ~32 files. |
| [`module-layout.md`](module-layout.md) | Why this stays one module until publication. |
| [`workflow.md`](workflow.md) | The development loop: gates, coverage, break-it testing, benchmarking. |

## Do one job

Each page is the smallest complete code for one task, a test that shows it
worked, and what goes wrong. Every Go block in them is copied from
[`examples/howto`](../examples/howto), and a test there fails if a page shows
code that no longer exists.

| Page | Job |
| --- | --- |
| [`tasks/n-plus-one.md`](tasks/n-plus-one.md) | Stop an N+1 with `loader`, and see that it stopped. |
| [`tasks/authorization.md`](tasks/authorization.md) | Guard a field and a type; refuse, null or drop. |
| [`tasks/custom-scalar.md`](tasks/custom-scalar.md) | Add a custom scalar, by hand or from `MarshalGQL`. |
| [`tasks/testing-a-server.md`](tasks/testing-a-server.md) | Test the schema, a query, and the server over HTTP. |

## What is missing

Stated so nobody reads silence as completeness. Compared with a mature
project's documentation site, this has a reference, four task pages and no
curriculum. The next pages worth writing, in the order a user hits them:

- errors: what the engine produces, and what `WithErrorPresenter` is for
- cursor pagination beyond the `relay` package's godoc
