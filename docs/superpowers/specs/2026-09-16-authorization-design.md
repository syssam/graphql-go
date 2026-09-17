# Authorization, field control and stream interception

- **Date:** 2026-09-16
- **Module:** `github.com/syssam/graphql-go`
- **Status:** Approved design; not yet implemented
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
    Coord    Coordinate
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
