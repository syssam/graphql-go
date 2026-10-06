# Scope for an external security review

[`production-readiness.md`](production-readiness.md) names an independent
review as one of the things only someone outside this repository can supply.
This page is what to hand that reviewer: where to look, what an internal pass
already found there, and what nobody has examined. It is written so that the
review spends its time on what is new.

## In scope

| Area | Files | The property to break |
|---|---|---|
| Authorization | `authz*.go`, the auth paths of `exec.go` and `exec_object.go` | A guarded field or type is never served without its guard, by any route: interface, union, fragment, alias, list, subscription, `_entities`. A decision is never shared between principals or requests. |
| Input coercion | `coerce.go`, `input.go`, `internal/jsonr` | A literal and a variable carrying the same value are coerced alike. No input makes work or memory grow faster than its own size. |
| Validation and planning | `overlap.go`, `plan.go`, `plan_metrics.go`, `limits.go`, `document_limits.go` | With the documented limits set, one request cannot make the server do unbounded work, and a cached plan is safe for concurrent requests. |
| Transports | `transport/`, `internal/httpreq`, `internal/gqlwsproto` | The CSRF rule, the body limit and the origin check hold on all six handlers. A client that stops reading, or sends a malformed frame, costs a bounded amount. |
| Safelist | `ext/apq`, `ext/trusted` | With a trusted store configured, no request shape executes text that was not registered. |

Out of scope: `codegen/` and `cmd/gqlc` (they run at build time on the
author's own schema), the examples, and the benchmarks.

## What an internal review already found

In October 2026 each area was reviewed against its rule file in
`.claude/rules/`, with every finding reproduced by a test before it was fixed.
The classes, so a reviewer can look for their neighbours rather than for them:

- **Work that grew faster than the request.** The merge rule was quadratic for
  one fragment under many aliased fields; the `@authorizeInput` walk was
  quadratic in nesting depth; a refused operation was walked again on every
  request; an invalid deeply nested variable built an error path per level.
  Each is now linear and held by a test that counts steps or allocations
  (`TestMergeRuleWorkGrowsWithTheDocument`,
  `TestAuthorizedInputWalkIsLinearInNestingDepth`,
  `TestARefusedOperationIsNotWalkedAgain`,
  `TestAnInvalidVariableIsRefusedInSpaceLinearInItsDepth`).
- **Memory the budget did not count.** The plan cache charged query text
  while holding the parsed document and the plans. It charges all three now
  (`TestThePlanCacheBoundsWhatLargeDocumentsHold`).
- **A plan as large as fields times types.** A field selected on an interface
  is planned once per type it could be, which no limit counted.
  `WithMaxPlanFields` refuses such a plan before it is built
  (`plan_fields_test.go`). It is off by default, like the other limits.
- **State shared between requests.** A literal argument bound to a pointer was
  decoded once and handed to every request; a memo key for a fragment could
  equal a field's past 540 selections and return the wrong selection
  (`TestALiteralArgumentACopySharesIsDecodedPerRequest`,
  `TestASpreadAndANodeNeverShareAFingerprint`).
- **A check on the text received, not the text run.** A mutation an
  interceptor swapped in ran over GET (`TestQueryOnlyRefusesAMutation`, and
  the rows of `transport/equivalence_test.go` that name GET).
- **Output trusted from the author's code.** A custom scalar that wrote no
  value, or two, produced an invalid response
  (`TestCustomScalarMarshalMustWriteOneValue`).

No route was found that serves a guarded field without its guard, and no
request shape was found that runs unregistered text against a trusted store.
Both were probed, not proved.

## What has not been examined

Stated so that silence is not read as a result.

- HTTP/2 paths, and Fiber with `StreamRequestBody` or a non-default body limit.
- Load above about 150 concurrent connections, and anything on Windows.
- `fed.SubgraphFS`, `@requires`, entity interfaces and `@interfaceObject`.
- Timing side channels in authorization: whether a denied field and a missing
  one can be told apart by latency.
- The fuzz targets beyond the 30 seconds each that CI gives them.
- Every figure in `performance.md` under hostile input rather than benchmark
  input.

## What the review should leave behind

A report committed under `docs/`, naming each finding with a request that
reproduces it, and a test in the tree for each one that is fixed. A finding
that is declined is recorded with the reason, as `@defer` and `@stream` are
in `CLAUDE.md`.
