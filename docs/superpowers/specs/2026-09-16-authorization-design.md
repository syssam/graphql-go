# Authorization, field control and stream interception

- **Date:** 2026-09-16
- **Module:** `github.com/syssam/graphql-go`
- **Status:** Plan 1 (P1, P2, P3, P5, P7) implemented and merged (`e57e820`); Plan 2 decomposed in §9, 2a implemented on `feat/authz-inherited-requirements`, 2b implemented on `feat/authz-argument-sites` (see the Deviations note at the end of §9.4)
- **Files:** `plan.go`, `exec.go`, `exec_object.go`, `directive.go`, `subscription.go`,
  `schema.go`, `introspection.go`, new `authz.go`, new `ext/authz/`

## 1. Background and Problem Statement

A production consumer (a 5,455-type ERP schema) is evaluating a move off gqlgen.
Its authorization surface is large and already load-bearing:

| Mechanism | Scale | Where it lives today |
|---|---:|---|
| `@auth(requires: [String!])` | 1,589 SDL sites | gqlgen directive |
| `@sensitive` (PII audit) | 357 SDL sites (234 on inputs) | gqlgen directive |
| Role-driven hidden / masked / readonly fields | every field of every type | 764-line `AroundFields` |
| Operation gates (auth, app scope, mutation scope, query scope, subscription scope) | 6 | `AroundOperations` |
| `@cost` | 49 sites | folded into a complexity limit |

The motivating complaint is generation cost, and that part is already answered.
Running `codegen.Generate` over the consumer's 825 SDL files (301,552 lines,
5,455 types) produced 799 files / 274,438 lines in **9.5 s at 280 MB peak heap**,
against gqlgen's **1,988,983 lines** for the same schema — because this
generator loads no Go packages, while gqlgen type-checks the two million lines
it previously emitted.

The design question is what happens to authorization. The cheap answer is to
port the gqlgen shape: directives wrap resolvers, one global field middleware
does the rest. That answer is wrong here, and §2 shows why.

### 1.1 Three defects in the naive port, all demonstrated

**A field directive never runs on a subscription root field.** `subscription.go:250`
documents that the per-event writer substitutes the root field's executor, and
`fd.subscribe` is a separate function from `fd.anyResolve`, which is what
directives wrap. Measured against a schema with `@auth` on the subscription root:

```
@auth ran on subscription root: false
```

The consumer has 12 subscription fields carrying `@auth`. A port would silently
drop authorization on all of them.

**A directive on a pure field receives no `FieldContext`.** `exec_object.go:107`
skips attaching one when `fd.pure && len(fieldInterceptors) == 0`, and
`fd.wrap()` (`object.go:86`) does not clear `pure`. Measured with `@sensitive`
bound on one pure field and one resolver field:

```
@sensitive saw: [<nil FieldContext> User.ssn]
```

The consumer's PII audit calls `graphql.GetFieldContext(ctx)` to identify what
was read, and PII fields (`email`, `salary`, `birthDate`) are predominantly
struct-bound pure fields. The audit would lose the field identity it exists to
record.

**Field interception is not free.** A no-op field interceptor over a six-field
query, `-count=10`:

```
BenchmarkPlain-20                  ~850 ns/op   1008 B/op   18 allocs/op
BenchmarkWithFieldInterceptor-20  ~2100 ns/op   2258 B/op   44 allocs/op
```

roughly +190 ns and +4.3 allocs per field, paid by every field whether or not
it is controlled. gqlgen's `AroundFields` has the same character, so this is not
a regression — but reproducing it forfeits the one structural advantage this
engine has.

### 1.2 The advantage a port forfeits

gqlgen and graphql-js both execute against the document. This engine compiles a
document into an immutable, cached `plan` (`plan.go:36`), and `planField.exec`
is per-plan — `runSubscriptionEvent` already exploits this by copying a
`planField` and replacing its executor.

Authorization decomposes along that seam:

| | What | When | Depends on principal? |
|---|---|---|---|
| **Shape** | what the operation touches | plan compile, once | no |
| **Decision** | what this principal may do with it | per request, once | yes |
| **Enforcement** | applying it | at the write, by dense index | — |

Because the shape does not depend on the principal, the plan cache is not
multiplied by policy cardinality, and `planKey` (`plan.go:30`) stays as it is.

## 2. Prior Art

This design follows established practice rather than inventing a model. What
each source contributes, and where it is wrong for this engine:

**graphql-js** ships no authorization and
[tells you not to put it in the GraphQL layer](https://www.graphql-js.org/docs/authorization-strategies/):
"authorization should be handled in your business logic layer, not your GraphQL
resolvers", and "GraphQL.js doesn't interpret directives by default, they're
just annotations." The boundary of that advice matters: a business layer cannot
see which fields the client selected, so it cannot implement field masking,
filter-argument inference leaks, or default-deny coverage. The correct reading
for a framework is that `Authorizer` must be a *thin adapter to an external
decision point*, not a policy language of its own.

**envelop's `useOperationFieldPermissions`** extracts the selected schema
coordinates during the validation phase and rejects the operation. This is the
same idea as `AuthShape`, already shipped and in production use, and its
documentation states the two-tier model explicitly: the plugin and
resolver-level authorization "are complementary".

Its known failure mode is instructive.
[envelop#892](https://github.com/graphql-hive/envelop/issues/892): the plugin
skipped its check for introspection documents, so `query { __schema { __typename } greetings }`
bypassed field permissions entirely. This engine is structurally immune —
`noIntrospectionRule` (`introspection.go:13`) is a per-field
`observers.OnField` rule with no whole-document shortcut — but immunity must be
pinned by a test, not assumed.

**Pothos `scope-auth`** is the most complete general-purpose design in the
ecosystem. It contributes four requirements this design had missed: type-level
scopes evaluated once per object instance rather than once per field; `$any` /
`$all` composition; `skipTypeScopes` escape hatches; and `grantScopes`,
capability propagation from a parent to its subtree.

`grantScopes` addresses a problem the consumer has already documented in its
own config: `employee(id)` carries `@auth`, but the same row is reachable
through the employees connection, `node(id)`, and any edge resolving to an
`Employee` — "a directive on one query field governs none of those". Their
workaround is a forced resolver with a hand-written guard on every path.
Capability propagation is the principled form, and in a plan-compiled engine the
static part of it is free: the plan is a tree, so the grants in effect at a node
are a compile-time property of the path from the root.

**gRPC and Google AIP** contribute three things GraphQL sources do not:

`google.golang.org/grpc/authz` implements **both** `UnaryInterceptor()` and
`StreamInterceptor()`. The industry answer to defect A is not to patch the
directive into the subscribe path; it is that a stream is a different
interception shape and the framework says so.

The same package takes policy as versioned, hot-reloadable **data**
(`NewStatic(json)`, `NewFileWatcher(file, refresh)`, `OnPolicyUpdate`). That
supplies the decision-cache key: a policy version, not a hash of permission
sets. The consumer's own `permcache` (60 s TTL, Redis fan-out invalidation)
already emits that signal.

[AIP-211](https://google.aip.dev/211) settles three open questions:

> Services **must** check authorization before validating any request.

> `Permission '{p}' denied on resource '{r}' (or it might not exist).`

> [A service] should still only check for authorization applicable to the
> operation being called, and **should not** try to "help out" by checking for
> related authorization.

The second removes a knob: there is no choice between `PERMISSION_DENIED` and
pretending the field does not exist. There is one outcome with deliberately
ambiguous wording.

### 2.1 Reconciling "authorize before validate" with validation-phase permissions

AIP-211 and envelop appear to disagree. They do not; they operate at different
granularities. GraphQL cannot know which fields are selected before parsing, so
the ordering resolves into three stages:

| Stage | Work | Source |
|---|---|---|
| 1. before parse | authenticate; coarse authorization (may this principal issue operations at all, of this kind, over this transport) | AIP-211 |
| 2. parse + validate | syntax and schema validation. **Errors here must disclose no more than stage 1 permits** | — |
| 3. after plan | field-level authorization | envelop |

Stage 2 was an open hole. `DisableIntrospection` did not close it: the default
gqlparser rules answer a typo by naming the correct field, type, argument or
input field, leaving the schema enumerable one request at a time. Fixed
independently in commit `86f15a9` (`DisableSuggestions`), which swaps all five
suggesting rules — gqlgen swaps two.

## 3. Goals and Non-Goals

### Goals

- Cover the case inventory in §4 with the smallest set of primitives that does so.
- Zero measurable cost on fields that declare no authorization.
- Authorization that cannot be bypassed by *how* a field is bound (pure vs
  resolver) or *how* it is reached (query vs subscription vs edge).
- A core package that expresses hooks and holds no opinion about policy;
  opinions live in `ext/authz`.
- Failures that are build-time wherever the information exists at build time.

### Non-Goals

- A policy language. `Authorizer` adapts to OPA, Cedar, OpenFGA, Casbin or a
  hand-written checker; this package defines none of them.
- Federation-aware propagation. The consumer is not a federated graph. `@policy`
  coordinates are carried in the shape so a future subgraph layer can read them.
- Row-level *data* scoping (which rows a tenant may see). That belongs in the
  query builder. P5 only guarantees a type cannot silently arrive with no
  declaration.
- `@defer` / `@stream`. Deliberately unsupported (`introspection.go:87`).
- Response caching. Not a framework concern; P2 exposes a decision version so a
  cache outside can key on it.

## 4. Case Inventory

The coverage criterion for this design. Grouped by the earliest point at which
the decision can be made, because that determines what may be compiled away.

Numbering is stable across revisions of this document. Cases 24 (`@defer` /
`@stream`) and 25 (federation propagation) were raised and moved to Non-Goals
in §3, which is why they do not appear below. Case 28 appears twice on purpose:
its static half is folded into the plan, its dynamic half cannot be.

### 4.A Static, SDL-declared (principal-independent)

| # | Case | Primitive |
|---|---|---|
| 1 | `@authenticated` | P1 |
| 2 | `@requiresScopes`, including AND/OR | P1 |
| 3 | `@policy` naming an external decision point | P1 |
| 4 | Default deny: an undeclared field fails `NewSchema` | P5 |
| 5 | Internal fields restricted to specific clients | P1 |

### 4.B Per request (principal-dependent, row-independent)

| # | Case | Primitive |
|---|---|---|
| 6 | Scope satisfaction | P2 |
| 7 | Role-driven hidden / masked fields (policy as data, not SDL) | P2 |
| 8 | Restricted fields barred from `where` / `orderBy` / `groupBy` | P1 + P2 |
| 9 | Readonly fields in mutation inputs | P1 + P2 |
| 10 | Operation-level rejection | P2 |
| 11 | Tenant isolation | data layer; P5 guarantees declaration |

### 4.C Per object instance

| # | Case | Primitive |
|---|---|---|
| 12 | Ownership ("your own salary") | P4 |
| 13 | ReBAC — `can(user, view, doc)` | P4 |
| 14 | List filtering: omit unreadable elements rather than null the field | P3 + P4 |
| 15 | Batching, or authorization becomes N+1 | P4 |
| 26 | Type-level checks evaluated once per instance, not once per field | P4 |
| 28 | Capability propagation (`grantScopes`) — dynamic part | P4 |

### 4.D Enforcement outcome

| # | Case | Primitive |
|---|---|---|
| 16 | deny / null / zero / redact / drop | P3 |
| 17 | Null bubbling when a non-null field is withheld | P3 |
| 18 | The error shape itself as an existence oracle | P3 (AIP-211) |
| 27 | `$any` / `$all` composition of requirements | P1 |
| 28 | Capability propagation — static part folded into the plan | P1 |
| 29 | A field opting out of its type's requirement | P1 |

### 4.E Cross-cutting

| # | Case | Handling |
|---|---|---|
| 19 | Introspection must not disclose fields the caller cannot use | P6 (opt-in) |
| 20 | Subscriptions: authorize at subscribe **and** per event | P7 + P2 |
| 21 | Plan cache and APQ must not leak across principals | structural — shape is principal-independent |
| 22 | Audit of PII reads and of authorization decisions | P1 supplies coordinates |
| 23 | Authorization must not defeat DataLoader batching | P4 is a loader |
| 30 | Introspection composed into a normal operation must not bypass checks | structural; pinned by test |
| 31 | Coarse authorization precedes parse/validate | §2.1 staging |
| 32 | Validation errors as a schema-enumeration channel | done — `86f15a9` |
| 33 | Policy is versioned, reloadable data | P2 |

## 5. Primitives

### 5.1 P1 — `AuthShape` (plan compile)

```go
// Requirement is an OR of ANDs: satisfied when every scope in any one group
// is held. The zero Requirement is satisfied by everyone. The shape mirrors
// Apollo's @requiresScopes(scopes: [[String!]!]!).
type Requirement struct{ anyOf [][]string }

type SiteKind uint8

const (
    SiteOutput     SiteKind = iota // a selected output field
    SiteObject                     // a whole object type; checked once per instance
    SiteFilterArg                  // a coordinate named in where/orderBy/groupBy
    SiteInputWrite                 // a coordinate written by a mutation input
)

// AuthSite is one position in a plan that may need a decision.
type AuthSite struct {
    Coord    string
    Field    *ast.FieldDefinition // nil when Kind is SiteObject
    Object   *ast.Definition
    Kind     SiteKind
    Requires Requirement // declared in SDL
    Grants   []string    // static grants in effect on the path from the root
}

// AuthShape is what an operation touches, independent of who is asking. It is
// computed once per compiled plan and cached with it.
type AuthShape struct{ /* ... */ }

func (s *AuthShape) Sites() []AuthSite // indexed by site index
func (s *AuthShape) Scopes() []string  // every scope named anywhere, for one PDP round trip
```

Each `planField` gains `authIdx int32`; `-1` means no decision is needed.

### 5.2 P2 — `Authorizer` (per request)

```go
// Authorizer turns an operation's shape into a decision for one principal. It
// runs once per operation, before any field resolves. A returned error rejects
// the operation.
type Authorizer interface {
    Authorize(ctx context.Context, shape *AuthShape, d *Decision) error
}

type Decision struct{ /* ... */ }

func (d *Decision) Set(site int, o Outcome) error
```

`*Decision` is passed in rather than returned so the framework owns the
allocation and sizes it from the shape.

### 5.3 P3 — `Outcome`

```go
func Allow() Outcome
func Deny(permission, resource string) Outcome // AIP-211 wording
func Null() Outcome
func Zero() Outcome
func Redact(fn func(any) any) Outcome
func Drop() Outcome // omit from the enclosing list
```

`Zero()` on a non-null composite field is rejected by `Decision.Set`, because
an object has no meaningful zero and the alternative is null-bubbling the
parent. The consumer discovered this the hard way — a hidden `totalAmount:
Decimal!` blanked a page. The site knows the field type at compile time, so
this is a returned error once per request, not a runtime surprise.

### 5.4 P4 — per-object decisions

The core package provides the per-object hook only:

```go
// ObjectAuthorizer decides authorization that depends on the resolved object
// rather than on the operation alone. It is called before an object's fields
// are written, once per object, for sites the shape marked as needing it.
//
// Implementations that block must Park on the OperationContext's
// WaveCoordinator, exactly as loader.Loader does, so that siblings batch.
type ObjectAuthorizer interface {
    AuthorizeObject(ctx context.Context, site int, obj any) (Outcome, error)
}
```

Batching uses the existing `WaveCoordinator` (`wave.go:57`), and `ext/authz`
builds a batched `Guard` on `loader.Loader`. No second batching mechanism is
introduced: a ReBAC check and a data fetch are the same scheduling problem.

"Once per object" is what makes case 26 fall out — a type-level requirement is
a site whose `Kind` is the object rather than a field, so its check is hoisted
above the field loop instead of repeating for each of the type's fields.

### 5.5 P5 — `RequireAuthCoverage()`

```go
func RequireAuthCoverage() SchemaOption // NewSchema fails on an undeclared field
```

With `@public` as the explicit exemption. This is the root cause of the
consumer's `CustomerAggregate` bypass: a type arrived that no role granted and
no role restricted, and ~40 aggregate surfaces returned hidden columns in full.
825 SDL files cannot be held by review.

### 5.6 P6 — introspection filtering (opt-in, default off)

`introType.fields` (`introspection.go:406`) is a pure field today. Filtering
per principal requires making it context-aware, which costs. Default off keeps
that cost off the common path; envelop#892 is the evidence that this
interaction is where bugs live.

### 5.7 P7 — `SubscriptionInterceptor`

```go
type SubscriptionHandler func(ctx context.Context, oc *OperationContext) (<-chan *Response, error)

// SubscriptionInterceptor wraps the opening of a stream. It is the
// stream-shaped counterpart of OperationInterceptor, following gRPC's split
// between UnaryInterceptor and StreamInterceptor.
type SubscriptionInterceptor interface {
    InterceptSubscription(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error)
}
```

Per-event authorization needs nothing new: each event builds its own
`OperationContext` and runs the whole operation chain, so P2 re-evaluates on
every event and a mid-stream revocation takes effect immediately.

## 6. Risks with Acceptance Criteria

**`execState` gains a `*Decision`.** `CLAUDE.md` records that `execState` and
`OperationContext` sit on a size-class boundary, and that an `atomic.Int64` on
`execState` measured +3.2 % B/op with its feature disabled. The cost of one more
pointer is **not assumed**. Acceptance: `unsafe.Sizeof` before and after, and
`benchstat` over interleaved `-count=10` runs showing no regression with
authorization disabled. If it regresses, the fallback is to pack into existing
padding or reach the decision through `oc`.

**The hot-path check must be free.** `if d != nil && f.authIdx >= 0` on a field
with `authIdx == -1` is an integer compare on an already-hot cache line.
Acceptance: a benchmark pinning it, not an argument.

**Plan compile grows.** `AuthShape` is built during `compilePlan`, which
`2026-09-16-plan-expansion-blowup-design.md` shows is already the expensive
phase for abstract selections. Acceptance: shape construction is O(plan size)
with no new traversal, measured on the consumer's schema.

## 7. Testing Strategy

Per `CLAUDE.md`: a test that still passes when the feature is broken was
agreeing with the code. Every primitive ships with the mutation that must turn
it red.

| Break on purpose | Must fail |
|---|---|
| Drop authorization from the subscribe path | `@requiresScopes` on a subscription root |
| `Decision.Set` becomes a no-op | every enforcement test |
| Coverage check always returns true | "schema with an undeclared field must not build" |
| Remove static grant folding | path-dependent authorization test |
| Make `Zero()` on a non-null composite succeed | null-bubbling test |
| Restore the suggesting validation rules | `TestDisableSuggestionsWithholdsSchemaDetails` |

Two pins beyond that:

- `query { __schema { __typename } restrictedField }` must be rejected — the
  shape that bypassed envelop#892.
- Authorization must hold identically for a pure `Field` binding and a
  `Resolve` binding of the same coordinate. Defect B existed precisely because
  those two paths differed.

## 8. Resolved Questions

**The consumer keeps `@auth(requires:)`; `ext/authz` ships the Apollo spelling.**
Migrating 1,589 sites to `@requiresScopes(scopes: [[String!]!]!)` is mechanical
but buys the consumer nothing it needs — it is not a federated graph, and the
only capability it gains is AND composition, which none of its 1,589 sites use.
P1 accepts any directive that yields a `Requirement`, so the core package is
indifferent. `ext/authz` binds the Apollo vocabulary (`@authenticated`,
`@requiresScopes`, `@policy`) so a new user gets the standard by default and an
existing one is not forced through a rename to adopt the engine.

**`SiteInputWrite` is split: coordinates at compile, values at decision time.**
The walk cannot be fully compiled, because a mutation's input object usually
arrives in a variable and the set of keys the client actually sent is not known
until the request. What *is* static is the input type rooted at each argument,
so plan compile records the root coordinate and its input type, and the decision
walks the supplied value against it. This matters for correctness, not just
speed: an omitted key is not a write, but a key sent as explicit null is, and
only the request can tell those apart.

### Still open

- Whether `Redact(fn)` should receive the `AuthSite` as well as the value. It
  is free to add later and speculative to add now; deferred until a caller
  needs it.

## 9. Plan 2

Plan 1 shipped the spine. Every item it deferred is an independent subsystem, so
Plan 2 is split rather than written as one plan, ordered by the consumer's need
and by how much fail-open each leaves in place.

| Sub-plan | Scope | Why this position |
|---|---|---|
| **2a** | Requirement inheritance (object type, implemented interfaces) and `__typename`; Authorizer error hardening | Closes the two paths Plan 1's docs state are *not enforced*. Build time and plan compile only. |
| **2b** | Argument sites: `SiteFilterArg`, `SiteInputWrite` | The consumer's field control needs both (cases 8, 9). |
| **2c** | `ObjectAuthorizer`, wave batching, `Drop()` | Instance-level authorization; the largest design. |
| **2d** | `ext/authz` (`@authenticated`, `@policy`, a batched `Guard`) | An opt-in convenience layer over 2a-2c. |
| later | Introspection filtering (P6), static grant folding | On demand. The consumer's "guard every path" need is met by 2a's inheritance, not by grants. |

### 9.1 Not planned: observing refused fields

`FieldObserver` (merged independently of this work) documents that a field an
Authorizer denied, nulled or zeroed is never observed. That stays. The place to
audit an authorization decision is the Authorizer, which sees every site and
every outcome it chose; an observer seeing "a field did not run" would add
nothing a decision log lacks.

### 9.2 2a — inheritance

**A field's effective requirement is computed once, at `NewSchema`**, as the AND
of: its own `@requiresScopes`; its object type's; each implemented interface
type's; and the same-named field on each implemented interface. It is stored on
the field's `fieldDef` and the object's effective type-level requirement on its
`objectType`. The shape builder and `RequireAuthCoverage` both read those stored
values, so the requirement authorization enforces and the one coverage accepts
cannot diverge. Every input is schema-level and immutable after build, so the
value is the same on every path that reaches the field, which is what keeps a
memoized, shared `*selectionSet` safe.

AND of two OR-of-AND requirements is their cross product. `NewSchema` fails when
an effective requirement exceeds 64 groups, so a pathological combination is a
build error rather than a per-request cost.

**`__typename` is guarded** by its object's effective type-level requirement,
through a `SiteObject` site. The reason is consistency: "every field of a guarded
type is authorized" should hold for `__typename` too, rather than leaving one
field every client selects as the exception. The compiler keys each
concrete selection set by its `*objectType`, so a `__typename` field is never
shared across object types and its site is well defined. `Decision.Set` admits
only `Allow` and `Deny` on a `SiteObject`: `__typename` is `String!`, so `Null`
would be a silent spec violation, and `Zero`/`Redact` have no field to act on.

**Known limit: field- and instance-level authorization does not hide how many
objects of a guarded type exist, or that they exist.** A correct denial reveals
the count itself: `{ list { name } }` answers `[null,null]` with one error per
element. An object whose selection folds to empty is written without any site
being consulted: `{ list { ... @include(if: false) { name } } }` answers
`[{},{}]` with no error and no authorization call. And a union selection that
names only an unguarded member, `{ mixed { ... on Open { name } } }`, answers
`[{"name":""},{}]`, revealing that some other member is present. Guarding
`__typename` does not change any of this and is not meant to. Making count and
existence confidential needs a decision at the field that returns the guarded
type, not at its fields, and is deferred to Plan 2c.

**`@requiresScopes` on a location the engine does not enforce is a build error**
(union, enum, scalar, input object, argument, input field). A silent no-op is
the failure this whole design exists to remove.

**`RequireAuthCoverage` counts a field as covered when its effective requirement
is non-zero**, or when the field or its object carries `@public`. `@public` on an
interface does not exempt implementers: exemption stays explicit per type.

**Authorizer errors that are not `*Error` are not shown to clients.** A policy
decision point's transport failure ("dial tcp 10.0.3.7:8181: connection
refused") would otherwise reach the client verbatim, disclosing internal
addresses and making an outage indistinguishable from a denial. Such an error
is presented as a generic internal error; the original is logged and kept as
the presented error's cause. An `*Error` the Authorizer built on purpose passes
through as before.

**Deviations (what shipped beyond the text above):**

- **Every `@requiresScopes` occurrence on a definition is ANDed**, not just the first
  match: `repeatable`, or an `extend type`/`extend interface`/`extend schema` re-declaring
  the directive, adds a second occurrence to the same `Directives` list rather than
  replacing the first, and gqlparser skips its own non-repeatable check for an extension
  occurrence even when the directive is not declared repeatable. `requirementOf`
  (`authz_shape.go`) reads and combines all of them.
- **The 64-group cap is checked before the product is built, not after**: `andCapped`
  predicts the resulting group count from the two operands' counts and refuses to call
  `Requirement.And` when the prediction exceeds `maxRequirementGroups`, because `And`
  itself allocates the full cross product unconditionally. A post-hoc check would still
  pay for that allocation — four interfaces of 30 groups multiply to 810,000 — on the way
  to reporting the error it exists to avoid.
- **Placement rejection is wider than the list above**: the schema definition itself and a
  directive definition's own argument are build errors too, alongside the union, enum,
  enum value, scalar, input object, input field and field-argument placements already
  named (`validateAuthDirectives`, `authz_shape.go`).
- **The Authorizer-error wrapper (`authorizerCause`, `exec.go`) deliberately has no
  `Unwrap`.** It supports `errors.Is` so a custom `ErrorPresenter` can still test the
  original cause, but a presenter that walks the chain with `errors.As` looking for an
  `ExtensionsProvider` must not reach the policy backend's own error and merge its
  extensions — an internal host, a trace ID — into the client-visible response.
- **The unguarded `__typename` fast path.** `writeFieldValue` (`exec_object.go`) checks
  `f.kind == fieldTypename && f.authIdx < 0` and returns `obj.name` before touching
  `execState` at all, because the naive ordering (check `execState` first) cost +6.26%
  (n=12) on a `__typename`-dense benchmark; reordering narrowed it to +1.35% (p=0.005,
  n=24, interleaved), which is the residual accepted as the cost of the object-site check
  existing at all.
- **Why `__typename` is guarded was corrected after the final review.** An earlier draft
  of this section justified the object site by saying an unguarded `{ items { __typename } }`
  counts a guarded type's rows and confirms a given one exists. Probes disproved that as a
  reason: the count and existence already leak through a correct denial, through an
  object whose selection folds to empty, and through a union's unguarded member (see the
  known limit above). The object site stays, for consistency, and hiding count and
  existence is Plan 2c's, decided at the field that returns the guarded type.
- **`ScopeAuthorizer` renders a requirement's structure in its denial**: groups joined by
  "or", scopes within a group by "and", a multi-scope group parenthesized only when there
  is more than one group. Flattening `Scopes()` with "or" told a client denied an
  inherited `{dog:read, pet:read}` that either scope would do.
- **A coordinate whose requirement hit the cap gets no coverage error.** Its stored
  requirement is zero only because the cap stopped it, so `RequireAuthCoverage` would
  otherwise add one "declares no authorization" error per field and bury the cap error.
- **A guarded `Query` type guards introspection.** `__schema` and `__type` are fields of
  the query root and inherit its requirement, so introspection is denied without the
  scope. That fails closed and is intended.

### 9.3 2b — argument sites (decided here, planned separately)

The engine cannot know which argument is a filter, nor that a key such as
`taxNumberContains` names the field `taxNumber` — that is the consumer's naming
convention. So:

- The declaration is in SDL: `@authorizeInput(kind: FILTER | WRITE)` on an
  argument definition. Schema-first, visible to `RequireAuthCoverage`, and
  emitted by a generator rather than hand-written.
- The engine reports, it does not interpret. At decision time it walks the value
  actually supplied (variables resolved) and hands the Authorizer the key paths
  that were present. Mapping a key to a field is the Authorizer's job.
- Only `Deny` is valid on an argument site, refusing the field before its
  resolver runs.
- An absent key is not reported; a key sent as explicit `null` is (§8).
- An operation whose shape has no argument site walks nothing.

### 9.4 2b — argument sites: decisions made before planning

Checked against the consumer before planning, and one §9.3 statement was
wrong: reporting only key paths does not cover ordering. A relay connection's
`orderBy` is `{field: CustomerOrderField!, direction}` — the restricted
column is named by an **enum value**, not a key, so `orderBy: {field:
TAX_NUMBER}` sorts by a hidden column and leaks it by bisection while every
key sent is innocuous. The consumer's own `field_control.go` walks keys for
`where`, `groupBy` and `having`, and enum values for `orderBy`.

**D1 — Declaration.** `directive @authorizeInput(kind: AuthorizeInputKind!)
on ARGUMENT_DEFINITION` with `enum AuthorizeInputKind { FILTER WRITE }`,
declared by the schema author like `@requiresScopes`. `NewSchema` fails when
the directive sits on an argument whose named type is not an input object or
an enum (lists of either allowed): there is nothing else to report.

**D2 — Sites.** For every selected field whose definition carries the
directive on an argument, plan compile adds one site per such argument:
`SiteFilterArg` or `SiteInputWrite`, `Coord` `Type.field(arg:)`, a zero
`Requires`, and the argument's name. The site exists whether or not the client
supplied the argument, so an Authorizer sees an empty input rather than a
missing site.

**D3 — What is reported.** Before `Authorize`, the engine walks each argument
site's **supplied** value — the operation's AST plus its variables, typed
against the schema — and exposes `Decision.Input(site) []InputKey`, where
`InputKey{Path []string; Enum string; Null bool}`:
- `Path` is the input-object keys from the argument down, list indices omitted.
- `Enum` is set when the value at that path is an enum literal: this is the
  correction above.
- `Null` is set when the value there is an explicit null; absent keys produce
  no entry.
- Scalar values (strings, numbers) are never reported: they are user data, and
  the policy decision needs only schema identifiers.
- Entries are deduplicated.
- Supplied means sent by the client. A default on an operation variable counts,
  because the operation author wrote it. A default on the argument or an input
  field in SDL does not, because the client did not choose it. This is why the
  walk reads the AST rather than `ArgumentMap`, which fills SDL defaults in.

**D4 — Outcomes.** Only `Allow` and `Deny` are valid on an argument site. `Deny`
refuses the field before its resolver runs, with the error at the field's path,
bubbling like any field error.

**D5 — Routing without touching the ordinary path.** A field with at least one
argument site is always given an output site (with a zero `Requires` if it
declares none), so the single existing `authIdx >= 0` compare routes it into
`enforceAuth`. There the field's argument sites — held on `planField.argSites`,
per plan — are checked first for a Deny, and only then is the output outcome
applied as before. A field with no argument site pays nothing new.
(Corrected from an earlier draft of this paragraph, which had the order
backwards; see item 6 of the Deviations note below for why that order
matters.)

**D6 — Subscriptions.** The open handler refuses the subscription when any
argument site on the root field is denied, as it already does for a Deny on the
root's output site.

**D7 — Cost.** The input walk runs only when the plan's shape has an argument
site. It is O(size of the supplied arguments), which coercion already paid.

**D8 — Default policy.** `ScopeAuthorizer` leaves argument sites at `Allow`:
their requirement is zero, and mapping keys or enum values to guarded fields is
the consumer's naming convention, not the engine's.

**D9 — Coverage.** `RequireAuthCoverage` does not require arguments to be
declared. The engine cannot tell which arguments are filters, and requiring a
declaration on every input-object argument would reject every schema.

### Deviations — what shipped on `feat/authz-argument-sites`

Implementation is complete and this section's decisions hold, with the
refinements and extensions below, found while building and benchmarking
against the code, not while planning.

1. **D3's `Enum` field, refined.** The walk reports `Enum` only when the
   schema type at that position is actually an `enum` in the SDL. A custom
   scalar accepts a bare identifier too — `SECRET` parses to the same AST
   `EnumValue` node whether its declared type is `enum Classification` or
   `scalar Classification` — so a bare identifier at a custom-scalar position
   is reported as a key only, `Enum` empty, never a fabricated enum value. An
   enum string arriving through a variable is reported only when it names a
   value the enum actually declares; one a value coercion would reject is not
   reported as if the client meaningfully chose it.
2. A literal object key whose *value* is a variable holding an object or a
   list is reported as a key, exactly as a literal object or list value at
   that key already was — the composite check looks at the variable's runtime
   value as well as the literal's AST kind, not the AST kind alone.
3. A null list element is not itself a reported position. Null means the
   position the list lives under is null, not that the list's contents are,
   so `{and: [null]}` reports `and` with `Null` false; the list element
   contributes nothing on its own. Keys read off a variable's map are reported
   in sorted order, so the result does not depend on Go's randomized map
   iteration.
4. Beyond D1: `NewSchema` also rejects an `@authorizeInput(kind: ...)` whose
   `kind` is not the bare enum literal `FILTER` or `WRITE`. gqlparser never
   type-checks a directive argument's literal against its declared type — the
   same gap `checkRequiresScopes` closes for `@requiresScopes`'s `scopes`
   argument — so an unrecognized name or a string literal in the `kind`
   position would otherwise build cleanly and plan compile would silently
   read it as `FILTER`.
5. Layout, not decided by D2/D5: `planField.argSites` is an `int32` count, not
   a slice — a field's argument sites are contiguous right after its own
   output site (`shape.sites[authIdx+1 : authIdx+1+int(argSites)]`) — because
   `runSubscriptionEvent` copies a `planField` by value once per event, and a
   slice header there pushed the struct from 176 to 200 bytes, into the next
   size class, paid on every event. `Decision`'s per-request input table sits
   behind a pointer (`src *decisionSource`); a shape with no argument sites
   shares one `decisionSource` cached on the `AuthShape` itself, so `Decision`
   stays 32 bytes and a request against a plan with no argument sites
   allocates no input table.
6. **D5's check order was backwards; corrected in place above.** The
   paragraph as first written applied the output outcome and only then
   checked argument sites. What ships checks argument sites first: an output
   `Null`, `Zero` or `Redact` must never be able to mask a denied argument,
   and `Redact` in particular must never run its resolver against input the
   policy refused.
7. Not decided by D3/D6: when a subscription's stream opens, the source is
   opened with its arguments decoded again from `oc.Variables` — the same map
   the Authorizer walked at open time — rather than reusing the early decode
   kept for interceptor visibility. Without this, a `SubscriptionInterceptor`
   that rewrites `Variables` before the source opens could show the policy
   one input and hand the source another; the early decode is kept only as
   up-front validation, so a malformed argument is still refused before any
   interceptor runs. Documented limitation: an `OperationInterceptor` that
   rewrites `Variables` on a single subscription *event* changes what that
   event's root argument sites are evaluated against, while the long-lived
   source stays bound to the arguments decoded when the stream opened.
   Accepted because interceptors are trusted server code, not the client.
8. Not anticipated by §9.3 or §9.4: gqlparser v2.5.37's
   `OverlappingFieldsCanBeMerged` compares two arguments' values only by
   `Kind` and `Raw`, and `Raw` is empty for both object and list literals. So
   `{ a: customers(where:{x}) a: customers(where:{y}) }` validates as one
   merged field, and the plan keeps only the first AST node's argument — the
   second literal is discarded by the validator before this feature ever sees
   the document. This is not an authorization bypass: `buildField` gives the
   merged `planField` a single `ast` (the first node), so the resolver and the
   Authorizer read the same argument value; there is no path by which they
   diverge.
9. Performance: interleaved n=12 against the pre-feature base, no significant
   change. `TypenameHeavy` 15.59µs vs 15.61µs (p=0.810); `Users` 1.840µs vs
   1.836µs (p=0.977); `ConcurrentList` 127.3µs vs 128.7µs (p=0.291); allocations
   equal in every sample (54/18/529). Struct sizes held: `execState` 64 bytes,
   `OperationContext` 160, `planField` 176.


### 9.5 2c — instance sites: decisions made before planning

§9 places `ObjectAuthorizer`, wave batching and `Drop()` in 2c and calls it the
largest design. What follows is decided, and an implementation plan may not
reopen it without recording a deviation.

The shape is taken from what the established systems do, because the consumer
asked for the industry standard rather than a novel mechanism:

- **graphql-ruby** checks an instance at the *type*: `authorized?(object,
  context)` runs whenever an object of that type is about to be returned, and
  an unauthorized object is *silently replaced with nil* unless
  `Schema.unauthorized_object` raises or substitutes. Its list pre-filtering is
  a separate hook (`scope_items`), not the same call.
- **ent** (Go) evaluates privacy rules returning Allow, Deny or Skip, and its
  `Filter` pushes a `Where` into the query, so bulk row filtering happens in
  the data layer.
- **Pothos** states that "can this user see THIS row" belongs in the loader or
  service rather than in field scopes, and that an unauthorized field returns
  null or a caller-supplied result.
- **OpenFGA/Zanzibar** filter a list by `BatchCheck`: narrow and sort in the
  database, then batch-check the page, with a correlation id per check and a
  bounded batch size (50 by default).

So: the hook is at the type, the default for an unauthorized instance is a
null rather than an error, checks are batched, and the docs must say that bulk
filtering belongs in the query when the data layer can express it.

**D1. `directive @authorizeObject on OBJECT` marks a type as instance-guarded.**
As in 2a and 2b, the marker is in the SDL so the shape is known at plan
compile and an unmarked type pays nothing. It takes no arguments: what the
policy needs to know is the type, the instance and the principal, all of which
it has. `@authorizeObject` on a location the engine cannot enforce is a build
error, as `@requiresScopes` is.

**D2. Instance decisions do not live in `Decision`.** A `Decision` is built once
per request, before any resolver runs, and cannot name instances that do not
exist yet. Instance outcomes come from a second, optional interface called
during execution:

```go
type ObjectAuthorizer interface {
    AuthorizeObjects(ctx context.Context, checks []ObjectCheck) ([]Outcome, error)
}

type ObjectCheck struct {
    Site   AuthSite // Kind SiteInstance, Coord the type name
    Object any      // the resolved Go value about to be written
}
```

It is registered with `WithObjectAuthorizer`, beside `WithAuthorizer`. The
returned slice is positional and must have the same length as `checks`; any
other length is an Authorizer error, not a silent partial allow.

**D3. The site is `SiteInstance`, a new `SiteKind`.** `SiteObject` stays what
2a made it: one type-level decision per plan, which guards `__typename`.
`SiteInstance` is recorded per *field position* that returns an
instance-guarded type, so the policy sees the coordinate that produced the
value. A field returning an unmarked type keeps the `authIdx` of -1 it has
today.

**`planField` cannot carry another field, so the index is derived.** The struct
is exactly full at 176 bytes: an added `int32` measures 184, and so does an
added `bool` (measured, both). So a field's sites keep the contiguous layout
2b introduced and extend it by one position — output site, argument sites,
then the instance site — and the high bit of `argSites` records that the
instance site is present. The instance site is then at
`authIdx + 1 + argSites&countMask`, and a field with an instance-guarded target
gets the zero-requirement output site that 2b already synthesizes for argument
sites, so `authIdx >= 0` routes it. Both halves of `argSites` must be read
through named helpers, never inline, and the count's mask must be asserted in
the same test that pins the struct size.

**D4. Batching follows the existing wave boundary.** `writeListConcurrent`
already drains a list's elements before spawning tasks, and `pushWave`
announces the sibling count that makes DataLoader batching work. An
instance-guarded list issues one `AuthorizeObjects` call for the whole drained
list before any element is written; an inline list batches per list; a single
object is a batch of one. Batches are split at `WithObjectAuthBatch(n)`,
default 50, matching OpenFGA's default, and the splits run sequentially — a
policy that wants more parallelism has the whole batch in one call and may do
as it likes with it.

**D5. Only Allow, Null, Deny and Drop are valid for an instance site.** `Zero`
and `Redact` are leaf outcomes and have no meaning for an object. `Null`
writes null, which is graphql-ruby's default and therefore this engine's:
**an `ObjectAuthorizer` returning the zero `Outcome` allows**, as elsewhere, so
a policy must say `Null()` deliberately. `Deny` produces the ordinary
`CodeForbidden` error at the field's path.

**D6. `Drop` omits the value from its enclosing list**, and is finally
implemented here. It is valid only at a list element position; on a bare field
it is an Authorizer error, as `Drop` on a non-list is meaningless. Dropping
from `[T!]!` is allowed: the result is a shorter list, which is what AppSync
and Hasura produce, and refusing it would make the commonest guarded case —
a non-null element list — the one case the feature cannot express. Two
consequences are documented rather than fixed: a client cannot tell a filtered
list from a short one, and **the response's list indices renumber**, so an
error reported for a later element names its written index, not its index in
the source data.

**D7. A null on a non-null element position bubbles as it always has.** `Null`
inside `[T!]!` nulls the list, then the field, by the existing rules. That is
the reason `Drop` exists, and the docs must show the pair together.

**D8. Existence hiding is exactly what `Drop` gives, and nothing more.** §9.2's
known limit stands for everything else: a guarded single object still answers
null, an empty selection still answers `{}`, and a union that names only an
unguarded member still reveals that some other member is present. A field that
must not disclose a count filters in its own query — the engine cannot know
what "the same list, filtered" means for the caller's data source.

**D9. `ObjectAuthorizer` errors are hardened exactly as `Authorizer` errors
are.** A non-`*Error` failure is presented as a generic internal error with the
original behind the unexported cause, and a panic is recovered into one. A
batch that fails fails every check in it: no instance in that batch is written.

**D10. Subscriptions check instances per event.** The root object and every
guarded object below it go through the same path the query executor uses, so a
principal who loses access mid-stream stops seeing those rows on the next
event.

**D11. Cost.** A plan that selects no instance-guarded type must measure
unchanged, and `execState` (64 bytes), `OperationContext` (160) and `planField`
(176) must all hold — see D3 for why the last of those forces the derived
index rather than a new field. An instance-guarded plan pays one
`AuthorizeObjects` call per wave plus the slice of checks it carries; the
check slice is built only when a wave contains a guarded value. No reflection
is added to the write path.

**D12. Out of scope for 2c**, to be taken up only if the consumer needs it:
per-`(field, instance)` masking, where an instance decision changes a *field's*
outcome rather than the object's; `scope_items`-style pre-filtering hooks; and
static folding of instance checks into the data source's query. The engine's
answer to bulk filtering stays "push it into the query", documented beside
`Drop`.
