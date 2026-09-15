# graphql-go: Design Specification

- **Date:** 2026-09-11
- **Module:** `github.com/syssam/graphql-go`
- **Status:** Approved design; Phase 1 engine is implemented
- **Minimum Go version:** 1.27

## 1. Background and Problem Statement

`99designs/gqlgen` is the de-facto schema-first GraphQL framework for Go. Its
runtime is fast and its feature set is broad, but on large schemas (hundreds
of entities, thousands of types) its build pipeline becomes the dominant cost
of the development loop:

1. **Whole-module type loading at codegen time.** gqlgen uses
   `golang.org/x/tools/go/packages` with `NeedSyntax | NeedTypesInfo` to bind
   schema types to Go types (autobind, resolver matching). On a large module
   this step takes minutes and several GB of RAM.
2. **One function group per field, in a single package.** The generated
   `generated.go` contains `_Type_field`, `fieldContext_Type_field`,
   `marshalN*`/`marshalO*`, `unmarshalInput*` and complexity functions for every
   field. Large schemas produce hundreds of thousands of lines in one package.
   The Go compiler's front end (type checking, SSA construction) is effectively
   serial within a package, so compile time and memory grow at least linearly
   with that single file.
3. **No incremental generation.** Any schema change regenerates and recompiles
   everything.

A concrete motivating case: an Ent-compatible ORM (`syssam/velox`) that
already generates one small Go package per entity to keep incremental
rebuilds fast. With 200+ entities its GraphQL extension emits a
multi-megabyte SDL plus a `gqlgen.yml`, and gqlgen then re-introduces the very
monolith the ORM avoided.

This document specifies a new, clean-slate GraphQL framework for Go whose
architecture removes those three costs while matching or exceeding gqlgen's
runtime performance and covering the full GraphQL specification.

## 2. Goals and Non-Goals

### Goals

- **Full GraphQL specification compliance** (October 2021 edition in v0.1,
  working-draft features in v0.2; see Section 9).
- **Production grade**: HTTP/WS/SSE transports, APQ, complexity limits,
  tracing, error masking, panic recovery.
- **Build performance that scales linearly and incrementally** with schema
  size: generated code is small, split into many packages, and generation
  never type-checks the whole module.
- **Runtime performance at or above gqlgen**: zero-reflection hot path,
  streaming JSON output, cached query plans, bounded concurrency.
- **One type-safe binding API** shared by generated code and hand-written
  code, so codegen is a convenience rather than a requirement.
- **Drivable by external generators** (ORMs such as Velox) through a
  programmatic manifest, with zero Go type loading in that mode.

### Non-Goals

- gqlgen API or configuration compatibility (clean slate).
- GraphQL gateway / schema stitching.
- Legacy `subscriptions-transport-ws` protocol.
- A custom GraphQL parser in v0.1 (`vektah/gqlparser/v2` is used behind an
  internal boundary).

## 3. Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Architecture | Schema-first runtime engine + code-first type-safe binding API as substrate + thin per-group codegen | Removes gqlgen's codegen/compile costs while keeping SDL as the contract and compile-time safety where Go can provide it |
| Parser / validator | `github.com/vektah/gqlparser/v2`, wrapped by internal interfaces | Battle-tested spec validation; plan cache makes parser speed irrelevant in steady state; replaceable later |
| Codegen engine | `github.com/dave/jennifer`, parallel per group, content-hash skip | Same approach that made Velox fast; auto-managed imports; streaming writes |
| WebSocket library | `github.com/coder/websocket` | Context-aware, maintained, minimal API |
| Type binding safety | Compile time for Go-expressible errors; `NewSchema` time for SDL-shape compatibility | Generated code is always shape-correct; hand-written code validates in one test |
| Minimum Go | 1.27 | Generic methods (`Omittable.Or`, `Selection.Collect`); `reflect.TypeFor` |

## 4. Architecture Overview

```
                  ┌────────────────────────────────────────────────────────┐
                  │ transport/gqlhttp  transport/gqlws  transport/gqlsse   │
                  └───────────────┬────────────────────────────────────────┘
                                  │ Request
                                  ▼
 ┌──────────────┐   ┌──────────────────────────────────────────────────────┐
 │  SDL files   │──▶│ graphql.Schema                                       │
 │ (embedded)   │   │  • ast.Schema (gqlparser)                            │
 └──────────────┘   │  • type registry: (GraphQL type, Go type) → adapters │
 ┌──────────────┐   │  • compiled field executors (+ directives/intercept) │
 │ bindings     │──▶│                                                      │
 │ (generated   │   └──────────────┬───────────────────────────────────────┘
 │  or written) │                  │
 └──────────────┘                  ▼
                    ┌──────────────────────────────┐   ┌──────────────────┐
                    │ graphql.Executor             │──▶│ plan cache (LRU) │
                    │  parse → validate → Plan     │   └──────────────────┘
                    │  execute Plan → []byte       │
                    └──────────────────────────────┘
```

### 4.1 Package Layout

```
github.com/syssam/graphql-go              package graphql — public core and engine
  │   schema.go, source.go, registry.go      schema construction and type registry
  │   scalar/enum/object/abstract/input.go   binding API and shape validation
  │   plan.go                                operation → Plan compiler, LRU cache
  │   exec*.go                               executor: scheduling, null bubbling, errors
  │   introspection.go                       __schema / __type implemented with the public binding API
  ├─ loader                               DataLoader over WaveCoordinator
  ├─ internal/jsonw                       allocation-free JSON writer
  ├─ codegen                              library: Config, Manifest, Generate
  │   └─ cmd/gqlc                         thin CLI over the library
  ├─ transport/gqlhttp                    GraphQL over HTTP (POST/GET), APQ, batching
  ├─ transport/gqlws                      graphql-transport-ws
  ├─ transport/gqlsse                     GraphQL over SSE (distinct connections mode)
  ├─ ext/complexity                       static + dynamic complexity and depth limits
  ├─ ext/otel                             OpenTelemetry tracing and metrics
  ├─ benchmarks/                          synthetic schema generator, gqlgen baseline, run.sh
  └─ docs/                                architecture, benchmarks, getting started
```

Dependency policy: the root package depends only on `gqlparser/v2` and the
standard library. Transports and extensions live in sub-packages so their
dependencies are opt-in.

The plan compiler and executor live in the root package rather than in
`internal/` sub-packages: the generic binding constructors must produce the
engine's field-executor values directly, and Go's import direction would
otherwise force every engine type to be re-exported through aliases. The
engine is still split into focused files, and only the JSON writer, which has
no dependency on engine types, is a separate internal package.

## 5. Public API: Binding Surface

Generated code and hand-written code use exactly the same API. The codegen's
only job is to write these calls from SDL plus Go type information.

### 5.1 Schema Construction

```go
sch, err := graphql.NewSchema(graphql.SDLFS(sdlFS, "schema/*.graphql"),
    graphql.Object[model.User]("User",
        graphql.Field("id",  func(u *model.User) graphql.ID { return graphql.ID(u.ID) }),
        graphql.Field("bio", func(u *model.User) *string { return u.Bio }),
        graphql.ResolveArgs("posts",
            func(ctx context.Context, u *model.User, a PostsArgs) ([]*model.Post, error) {
                return r.Posts(ctx, u, a)
            }),
    ),
    graphql.Query(
        graphql.ResolveArgs("user", func(ctx context.Context, _ graphql.Root, a UserArgs) (*model.User, error) {
            return r.User(ctx, a.ID)
        }),
    ),
    graphql.Args[PostsArgs](
        graphql.InputField("first", func(a *PostsArgs, v *int) { a.First = v }),
        graphql.InputField("where", func(a *PostsArgs, v *filter.PostWhereInput) { a.Where = v }),
    ),
    graphql.Input[filter.PostWhereInput]("PostWhereInput", /* InputField setters */),
    graphql.Enum[model.Role]("Role", map[model.Role]string{model.RoleAdmin: "ADMIN", model.RoleUser: "USER"}),
    graphql.Scalar[time.Time]("Time", marshalTime, unmarshalTime),
    graphql.Interface[model.Node]("Node"),
    graphql.Object[graphql.Root]("Subscription",
        graphql.SubscribeArgs("postAdded",
            func(ctx context.Context, _ graphql.Root, a PostAddedArgs) (<-chan *model.Post, error) { ... }),
    ),
)
```

`graphql.Source` is produced by `graphql.SDL(string)`, `graphql.SDLBytes([]byte)`
or `graphql.SDLFS(fs.FS, patterns...)`. Multiple `Object` bindings for the same
GraphQL type are merged (this supports `extend type Query` spread across
generated group packages); a duplicated field is an error.

### 5.2 Binding Constructors

| Constructor | Signature | Semantics |
|---|---|---|
| `Object[E]` | `(name string, fields ...FieldOption) SchemaOption` | Binds GraphQL object `name` to Go element type `E` (not `*E`). Values flow as `*E`. |
| `Field[P, R]` | `(name, func(P) R, ...FieldSchedule)` | **Pure** field: no context, no error, always executed inline, never scheduled on a goroutine. |
| `FieldArgs[P, A, R]` | `(name, func(P, A) R, ...FieldSchedule)` | Pure field with arguments. |
| `Resolve[P, R]` | `(name, func(context.Context, P) (R, error), ...FieldSchedule)` | **Resolver** field: may perform I/O; eligible for concurrent scheduling. |
| `ResolveArgs[P, A, R]` | `(name, func(context.Context, P, A) (R, error), ...FieldSchedule)` | Resolver with arguments. |
| `Subscribe[P, R]`, `SubscribeArgs[P, A, R]` | `(name, func(ctx, P[, A]) (<-chan R, error))` | Subscription root fields (Phase 3). |
| `Args[A]` | `(fields ...InputFieldOption) SchemaOption` | Decoder for argument struct `A`. Empty `Args[A]()` derives fields from struct tags. |
| `Input[T]` | `(name string, fields ...InputFieldOption) SchemaOption` | Binds GraphQL input object `name` to struct `T`. Empty field list is Auto. |
| `InputField[T, V]` | `(name string, set func(*T, V)) InputFieldOption` | Typed setter for one input field / argument. |
| `OmittableField[T, V]` | `(name string, set func(*T, graphql.Omittable[V]))` | Setter that distinguishes "absent" from "null". |
| `Enum[T comparable]` | `(name string, values map[T]string) SchemaOption` | Bidirectional mapping between Go values and GraphQL enum names. |
| `Scalar[T]` | `(name string, marshal func(*Writer, T) error, unmarshal func(any) (T, error)) SchemaOption` | Custom scalar. |
| `Interface[T]`, `Union[T]` | `(name string, opts ...AbstractOpt) SchemaOption` | Prefer a sealed interface for `T`. `TypeResolver` is required when one Go type backs several possible objects. |
| `Directive` | `(name, func(next FieldFunc) FieldFunc)` | No-argument schema directive. `OBJECT` wraps every field of the type. |
| `DirectiveArgs[A]` | `(name, func(next FieldFunc, args A) FieldFunc)` | Schema directive with an argument struct. |
| `Query` / `Mutation` / `Subscription` | `(fields ...FieldOption)` | Bind the schema's declared root type name. |
| `loader.New[K, V]` | `(BatchFunc[K, V], ...loader.Option)` | Per-request batch+cache, in `graphql-go/loader`. Concurrent `Load` calls in one executor wave share one batch. |
| `Root` | `type Root struct{}` | Parent value for root fields. |

`FieldSchedule` values: `graphql.Inline()` (force a `Resolve` field to run
synchronously), `graphql.Concurrent()` (allow a `Field` with heavy CPU work to
be scheduled). `FieldOpt` is an alias.

### 5.3 Type Registry and Shape Resolution

The registry is keyed by `(GraphQL named type, reflect.Type)`. Each
constructor registers typed adapters produced by generic instantiation, so the
same Go type may back different GraphQL types (`time.Time` for both `Time`
and `Date`) and the same GraphQL scalar may accept several Go types (`ID` as
`string` or `int64`).

For every registration, with `E` the non-pointer base type, the following
shapes are produced automatically:

- **Leaf writers** `func(*Writer, V)` for `V ∈ {E, *E, []E, []*E}` (scalars,
  enums).
- **Decoders** `func(any) (V, error)` for the same shapes (scalars, enums,
  input objects).
- **Traversers** for object types: `func(any, func(any))` iterating `[]E` and
  `[]*E` with typed loops.

`Field[P, R]` and `InputField[T, V]` resolve their adapters once at
`NewSchema` by looking up `(sdlType, reflect.TypeFor[R]())`. After that, every
request calls typed functions directly. Because the framework recommends
pointer values, boxing into `any` between executor and adapters does not
allocate.

Shapes that are not pre-registered (for example `[][]T`) fall back to a
reflection-based adapter and log a warning at startup. This is the only
reflection permitted on the request path and is expected to be rare.

### 5.4 Shape Compatibility Rules (checked at `NewSchema`)

`E` denotes the non-pointer Go base type bound to the SDL named type `T`.

| SDL type | Accepted Go shapes | Notes |
|---|---|---|
| `T!` | `E`, `*E` | A nil `*E` at runtime produces a field error and null-bubbling. |
| `T` | `E`, `*E` | `E` never yields null. |
| `[T!]!`, `[T!]` | `[]E`, `[]*E` | nil slice → `null` for a nullable list, error for a non-null list; nil element of `[]*E` → error. |
| `[T]!`, `[T]` | `[]*E` | Element nil → `null`. |
| interface / union | Go interface type or `any` | The dynamic type must be a bound object type. |
| input field `T` (nullable) | `*E`, `Omittable[*E]`, `Omittable[E]` | `Omittable` only via `OmittableField`. |
| input field `T!` | `E` | Default values from SDL are applied before setters run. |

Mismatches are collected and returned together from `NewSchema` as a single
multi-error, with schema coordinates (`User.posts`) in each message. A
`graphql.Validate(sdl, opts...)` helper exists for use in unit tests.

### 5.5 Supporting Types

```go
// Omittable distinguishes an absent input field from an explicit null.
type Omittable[T any] struct{ /* unexported */ }
func OmittableOf[T any](v T) Omittable[T]
func (o Omittable[T]) IsSet() bool
func (o Omittable[T]) Value() T
func (o Omittable[T]) ValueOK() (T, bool)
func (o Omittable[T]) Or(def T) T

// Writer is the streaming JSON writer handed to scalar marshalers.
type Writer struct{ /* pooled []byte */ }
func (w *Writer) String(string); Int(int64); Float(float64); Bool(bool); Null(); RawJSON([]byte)

type ID string

// FieldFunc is the type-erased field executor used by directives and field interceptors.
// args is the decoded argument struct pointer (*A) or nil for fields without arguments.
type FieldFunc func(ctx context.Context, parent any, args any) (any, error)

type Executor struct{ /* schema, plan cache, semaphore, interceptors */ }
func NewExecutor(s *Schema, opts ...ExecutorOption) *Executor
func (e *Executor) Execute(ctx context.Context, req *Request) *Response
// Subscribe is Phase 3; Execute rejects subscription operations.

type Error struct {
    Message    string
    Locations  []Location
    Path       Path
    Extensions map[string]any
    Err        error // wrapped cause, never serialized
}
type ErrorPresenter func(ctx context.Context, err error) *Error

type Request struct {
    Query         string
    OperationName string
    Variables     json.RawMessage
    Extensions    map[string]any // e.g. persistedQuery
}
type Response struct {
    Data       []byte // raw JSON or nil
    Errors     []*Error
    Extensions map[string]any
}
func (r *Response) Release() // returns pooled buffers
```

### 5.6 Resolver Context Helpers

- `graphql.SelectionFrom(ctx) graphql.Selection` returns a read-only view onto
  the plan node for the current field: `Has(name)`, `Fields()`, `Sub(name)`,
  `Args()`. No allocation; the plan is shared and immutable. This replaces
  gqlgen's `CollectFields` and is what ORM integrations use for eager-loading
  decisions.
- `graphql.PathFrom(ctx) Path` materializes the current response path (used in
  errors and tracing; allocates only when called).
- `graphql.OperationFrom(ctx) *OperationContext` exposes operation name, type,
  raw query, variables and a per-request `Values` map for extensions.

### 5.7 Interceptors

```go
type RequestInterceptor   interface { InterceptRequest(ctx context.Context, req *Request, next RequestHandler) *Response }
type OperationInterceptor interface { InterceptOperation(ctx context.Context, op *OperationContext, next OperationHandler) *Response }
type FieldInterceptor     interface { InterceptField(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) }
```

Request and operation interceptors wrap each request once and have negligible
cost. Field interceptors are registered on the `Executor` and baked into the
field executors when a `Plan` is compiled (the plan cache is per executor, so
each plan already knows its interceptor chain); when none are registered the
executor path contains no indirection. A field interceptor forces the
affected field's result through `any`, so field-level interception is
documented as opt-in and is used by `ext/otel` only when field spans are
enabled.

## 6. Execution Engine

### 6.1 Request Pipeline

1. **Document cache lookup.** Key = 64-bit `hash/maphash` of the query
   string; if `Extensions.persistedQuery.sha256Hash` is present, that hash is
   the key. Entries store the original query string and a hit is confirmed by
   string equality, so a hash collision can never return a foreign document.
   Variables are never part of the key.
2. **Miss path.** `gqlparser` parses and validates the document against the
   schema. The entry (default LRU of 1,024 documents, configurable) holds the
   validated document and a small map of compiled `Plan`s keyed by
   `(operation name, skip/include variant)`.
3. **Variable coercion.** Variables are decoded from `json.RawMessage` and
   coerced against the operation's variable definitions per request; the
   `@skip`/`@include` variant key is derived from them and the matching plan
   is compiled on first use.
4. **Execution.** The executor walks the plan and writes the response body.
5. **Presentation.** Errors pass through `ErrorPresenter`; the transport writes
   the envelope.

### 6.2 Plan

A `Plan` is an immutable tree produced once per (document, operation):

- Fragments are flattened. For abstract-typed parents the plan keeps one
  field set per possible concrete type, so runtime type resolution is a map
  lookup followed by a direct pointer to the right field set.
- Each field node holds: the typed field executor (already wrapped with schema
  directives at `NewSchema` and with the executor's field interceptors at
  plan time), pre-serialized `"alias":` bytes, the sub-selection, and
  non-null and list flags.
- **`@skip` / `@include` are constant-folded.** A parsed document records the
  set of Boolean variables referenced by `@skip`/`@include` (typically zero to
  three). The plan is specialized per combination of their values, and that
  bitmask is part of the plan-cache key beneath the document entry. Plans
  therefore contain no runtime conditions. Documents referencing more than 16
  such variables are compiled per request without caching.
- **Argument pre-decoding.** If a field's arguments contain no variables they
  are decoded into the typed `A` struct at plan time. At execution a shallow
  copy of that struct is passed to the resolver. Nested pointers inside are
  shared across requests and documented as read-only. Arguments that reference
  variables are decoded per request.
- Static complexity and depth are computed and stored on the plan for
  `ext/complexity`.

### 6.3 Output Writer

Responses are written directly into a pooled `[]byte` through
`internal/jsonw`; no intermediate `map[string]any` is built. Object and list
boundaries record their start offset. When a non-null position receives
`null` (nil value or resolver error), the writer truncates back to the
enclosing object's start offset, writes `null`, and reports the failure to
the parent, which repeats the process if it is itself non-null. This
implements specification null-bubbling with a single buffer and no
re-serialization.

Errors are appended to a mutex-protected slice. Each error records the plan
node and list indices; the JSON `path` is materialized only when the response
is presented.

### 6.4 Scheduling

- **Pure fields** (`Field`, `FieldArgs`) are always executed inline in the
  parent's goroutine.
- **Resolver fields** (`Resolve`, `ResolveArgs`) inside a selection set are
  scheduled concurrently only when the selection set contains two or more of
  them, or when iterating list elements whose sub-selection contains resolver
  fields.
- Concurrency is bounded by a per-`Executor` semaphore
  (`WithMaxConcurrency`, default `GOMAXPROCS × 4`). Acquisition is
  non-blocking: if no slot is available the field runs inline. This guarantees
  progress without deadlock and prevents goroutine storms on large lists.
- Each scheduled field writes into its own pooled sub-buffer. When all
  siblings complete, sub-buffers are spliced into the parent buffer in
  selection order and released.
- `Mutation` root fields execute strictly serially, in document order.
- Dataloaders batch naturally because sibling resolvers run concurrently; no
  level barrier is required. The framework does not ship a dataloader in
  v0.1; any context-scoped loader works.
- Context cancellation is checked between fields; a cancelled request stops
  scheduling new work and returns the partial result with an error.

### 6.5 Subscriptions

`Executor.Subscribe(ctx, req) (<-chan *Response, error)` validates and plans
the operation, invokes the `Subscribe*` binding to obtain the source channel,
and executes the subscription field's sub-selection once per event using the
same plan. The output channel closes when the source closes or the context is
cancelled. Transports map this to their protocol.

### 6.6 Errors and Recovery

- A resolver returning `error` produces a field error at the field's path.
  Errors implementing `interface{ GraphQLExtensions() map[string]any }` or
  wrapping `*graphql.Error` carry their extensions through.
- Every resolver invocation is wrapped in `recover`; a panic becomes an
  internal error with the stack logged via `slog` and a masked message
  (`"internal system error"`) unless the `ErrorPresenter` chooses otherwise.
- Validation and coercion failures return the standard `errors`-only
  response with `locations`.

### 6.7 Introspection

`introspection.go` binds `__Schema`, `__Type`, `__Field`,
`__InputValue`, `__EnumValue`, `__Directive` to `gqlparser`'s AST types using
the same public binding API. `graphql.DisableIntrospection()` rejects
`__schema` and `__type` selections at validation time. Introspection responses
participate in the plan cache like any other query.

## 7. Code Generation

### 7.1 Principles

- The library `codegen` is the primary interface; `cmd/gqlc` only reads
  `gqlc.yaml` and calls it. External generators import the library directly.
- Generation never type-checks the whole module. In **manifest mode** no Go
  type information is loaded at all. In **auto-bind mode** only the listed
  packages are loaded, with `NeedName | NeedTypes` (export data; no syntax,
  no function bodies).
- Generated code is split by **group** into many small packages. Files are
  written with Jennifer, in parallel per group, and only when their content
  hash differs from the file on disk, so `go build` caches remain valid.
- The generator never modifies files it did not generate. There is no
  gqlgen-style resolver stub rewriting.

### 7.2 Configuration

```go
type Config struct {
    SchemaGlobs []string          // SDL files
    Output      string            // directory for generated code
    Package     string            // import path of Output
    Manifest    *Manifest         // explicit bindings; may be nil
    AutoBind    []string          // package patterns for discovery; may be empty
    GroupFunc   func(typeName, sdlFile string) string // default: group = SDL file stem
    NullableInputOmittable bool   // use Omittable for all nullable input fields
}

type Manifest struct{ Types []TypeBinding }

type TypeBinding struct {
    Name   string                  // GraphQL type
    Go     GoType                  // {PkgPath, Name, Pointer}
    Group  string                  // overrides GroupFunc
    Fields map[string]FieldBinding // absent → discovery (auto-bind) or Resolver
}

type FieldBinding struct {
    Kind   FieldKind // StructField | Method | Resolver
    GoName string
}

func Generate(ctx context.Context, cfg Config) error
```

`gqlc.yaml` mirrors `Config` one-to-one for CLI users.

### 7.3 Binding Discovery (auto-bind mode)

For each SDL object field on a type bound to a Go struct, in order:

1. A struct field whose name matches case-insensitively, or whose `json` tag
   matches exactly → `StructField`.
2. A method with a matching name (case-insensitive) whose signature is
   `(ctx?, args?) (R[, error])` → `Method`.
3. Otherwise → `Resolver` (added to the group's `Resolver` interface).

Embedded structs and promoted methods are followed. Type aliases are
resolved through `go/types`.

### 7.4 Generated Output

```
<out>/
  schema.go                 root package: embeds SDL, Resolvers struct, NewSchema
  schema/*.graphql          copies of the SDL sources so go:embed can reach them
  model/                    Go structs and enums for SDL types without an existing Go type
    <group>.go              one file per group (declarations only; cheap to compile)
  <group>/
    bindings.go             Object / Input / Args / Enum registrations for the group's types
    args.go                 argument structs for the group's fields with arguments
    resolver.go             type Resolver interface { ... } for fields needing user code
```

- **SDL copies.** `go:embed` can only reference files below the package
  directory, so the generator copies every matched SDL file into
  `<out>/schema/` (hash-skipped like all other outputs). The originals remain
  the source of truth.

- **Models live in a single `model` package** because input objects and
  enums routinely reference each other across groups (`UserWhereInput` ↔
  `PostWhereInput`), which would create import cycles between group packages.
  The package contains only type declarations, which compile quickly even in
  large numbers. In manifest mode where all types already exist, `model` is
  empty and omitted.
- **Group packages** contain function bodies (one line per field) and import
  only the model packages their types need. Editing one entity recompiles its
  group, the root package and `main`.
- **`schema.go`**:

```go
// Code generated by gqlc. DO NOT EDIT.
package graph

//go:embed schema/*.graphql
var sdl embed.FS

type Resolvers struct {
    User user.Resolver
    Post post.Resolver
}

func NewSchema(r Resolvers, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
    all := []graphql.SchemaOption{
        user.Bindings(r.User),
        post.Bindings(r.Post),
    }
    return graphql.NewSchema(graphql.SDLFS(sdl, "schema/*.graphql"), append(all, opts...)...)
}
```

- Each group exposes `func Bindings(r Resolver) graphql.SchemaOption`, which
  returns the group's registrations with resolver fields closed over `r`.
  Groups with no resolver fields expose `Bindings()` without a parameter and
  are omitted from the `Resolvers` struct. Root `Query`/`Mutation`/
  `Subscription` fields are declared in the group that owns them using
  `graphql.Object[graphql.Root]` and merged at `NewSchema`.
- Argument structs are generated in the owning group (`args.go`) because only
  that group's bindings and `Resolver` interface reference them; they may
  import `model` for input object and enum types.
- Missing resolver methods surface as compile errors when the user's type is
  assigned to the `Resolvers` struct.

### 7.5 Size Estimate

For 2,000 types / 20,000 fields: roughly 20k lines of bindings plus models
and interfaces, spread across ~200 packages of 200–500 lines each; no single
file above ~2k lines. Generation target in manifest mode: full regeneration
under 2 seconds with no type loading. These figures are validated by the
benchmark suite (Section 10).

## 8. Transports and Production Features (v0.1)

### 8.1 `transport/gqlhttp`

- `POST` with `application/json`; `GET` with query parameters (queries only,
  mutations rejected per GraphQL over HTTP).
- Content negotiation for `application/graphql-response+json` and legacy
  `application/json`, with status codes per the GraphQL over HTTP
  specification.
- Automatic Persisted Queries: `extensions.persistedQuery.sha256Hash`, a
  `Store` interface with an in-memory LRU default, `PersistedQueryNotFound`
  error code.
- Optional request batching (JSON array).
- CSRF prevention: requests that would not trigger a CORS preflight must carry
  a designated header.
- Body size limit, decode timeout via context, `Content-Encoding: gzip`
  response support left to the surrounding server middleware.

### 8.2 `transport/gqlws`

- `graphql-transport-ws` protocol: `connection_init`/`connection_ack`,
  `ping`/`pong`, `subscribe`/`next`/`error`/`complete`.
- `OnConnect(ctx, initPayload) (context.Context, error)` hook for
  authentication; returned context is the parent of all operations on the
  connection.
- Per-connection subscription limit, keep-alive interval, init timeout.

### 8.3 `transport/gqlsse`

GraphQL over Server-Sent Events, distinct connections mode (one HTTP request
per operation). Supports queries, mutations and subscriptions.

### 8.4 Core Options

- `SchemaOption`: `graphql.DisableIntrospection()`; `graphql.PrintSDL(*Schema)`
  exports the schema as SDL.
- `ExecutorOption`: `WithMaxConcurrency(n)`, `WithPlanCache(size)`,
  `WithErrorPresenter(fn)`, `WithRecover(bool)`, `WithRequestInterceptor`,
  `WithOperationInterceptor`, `WithFieldInterceptor`, `WithMaxComplexity(n)`,
  `WithMaxDepth(n)`, `WithQueryCost(QueryCost)`.

### 8.5 Query cost and depth (core)

Enforced as an innermost `OperationInterceptor` before resolvers run, matching
GitHub's node/complexity limits and Shopify's query cost:

- Static complexity: 1 per selected field (aliases included).
- Depth: maximum selection nesting.
- Query cost: field weight (default 1, override via `FieldWeight["Type.field"]`)
  plus child cost × list size. List size is `first`/`last` or `DefaultListSize`
  (10). `Report: true` writes `extensions.cost`.
- `ext/complexity` remains reserved for result-based *actual* cost after
  execution (Shopify's `actualQueryCost`).

### 8.6 `ext/otel`

Request and operation spans by default; field spans opt-in (implemented as a
`FieldInterceptor`). Metrics: request duration, error count by code, plan
cache hit ratio, active subscriptions.

## 9. Specification Scope and Phasing

### v0.1 — GraphQL October 2021, complete

- All type kinds; interfaces implementing interfaces; unions; input objects.
- Custom scalars with `@specifiedBy`; repeatable directives; `@deprecated`
  on fields, enum values, arguments and input fields.
- Full input coercion including list coercion and default values; variables.
- `@skip` / `@include`; fragments and inline fragments with type conditions.
- Queries, mutations (serial), subscriptions.
- Complete introspection including `specifiedByURL`, `isRepeatable`, schema
  `description`, `includeDeprecated` on args and input fields.
- Error format (`message`, `locations`, `path`, `extensions`), null
  bubbling, partial results.
- GraphQL over HTTP specification.

### v0.2

- `@oneOf` input objects.
- `@defer` / `@stream` incremental delivery in the payload format of the
  working draft current at implementation time, over `multipart/mixed` and
  `graphql-transport-ws`.
- Apollo Federation v2 as `ext/federation`: `_service { sdl }`, `_entities`,
  `@key`, `@requires`, `@provides`, `@shareable`, `@external`,
  `@inaccessible`, entity resolvers via `graphql.Entity[T]`.
- GraphQL multipart request specification (file uploads).
- `graphql.InputStruct[T]` — reflection-derived input decoders for codegen-less
  usage.
- Custom executable (query-side) directives.

### Out of Scope

Gateway / stitching, legacy `subscriptions-transport-ws`, gqlgen
compatibility layer.

## 10. Testing and Benchmarks

### 10.1 Tests

- Unit tests in every package; the root package is covered by table-driven
  tests of the binding API and shape validation.
- Executor conformance suite ported from graphql-js executor tests and
  the specification's examples: execution order, coercion, null bubbling,
  error paths, abstract types, mutations, subscriptions.
- Codegen golden tests; CI additionally runs `go build` and `go vet` on the
  generated output so the compiler verifies binding correctness.
- Fuzz tests for `internal/jsonw` and input coercion.
- Transport tests against reference clients (`graphql-ws` protocol
  conformance, GraphQL over HTTP test cases).
- CI on Linux with `-race`, `golangci-lint`, and per-package coverage gates.

### 10.2 Benchmarks (`benchmarks/`)

- A synthetic schema generator produces N entities with Relay connections,
  `WhereInput` filters, order inputs and CRUD mutations, so 50 / 200 / 1000
  entities are measured with identical shapes.
- The same generated schema is built with gqlgen as the baseline.
- Metrics: codegen wall time and peak RSS; cold `go build` time and RSS;
  incremental rebuild after touching one entity; runtime RPS, ns/op and
  allocs/op for four representative operations (shallow list, deeply nested
  connection, mutation, full introspection).
- Results are published in `docs/benchmarks.md` and reproducible with
  `benchmarks/run.sh`.

## 11. Implementation Phasing

The v0.1 scope is delivered in four phases; each phase ends with passing
tests and a runnable example.

1. **Core and engine.** Root package binding API, type registry, shape
   validation, plan compiler, executor (queries and mutations),
   `internal/jsonw`, introspection, `transport/gqlhttp` without APQ. Exit
   criterion: a hand-written example schema passes the execution conformance
   suite.
2. **Codegen.** `codegen` library (manifest and auto-bind modes), `cmd/gqlc`,
   golden tests, generated-output compile check in CI. Exit criterion: the
   phase-1 example is regenerated from SDL with no hand-written bindings.
3. **Subscriptions and streaming transports.** `Subscribe*` bindings,
   `Executor.Subscribe`, `transport/gqlws`, `transport/gqlsse`, APQ in
   `transport/gqlhttp`.
4. **Production extensions and benchmarks.** `ext/otel`, result-based actual
   query cost, synthetic schema generator, gqlgen baseline, `docs/benchmarks.md`.
   Static complexity, depth and requested query cost already live on `Executor`.

### Phase 1 Deviations

Implementation of phase 1 settled the following details differently from the
text above; the text is kept as the design rationale and this list is the
authoritative behaviour.

- **`Object[E]` takes the element type.** `Object[User]`, not
  `Object[*User]`; a pointer type parameter is rejected at `NewSchema` with a
  message naming the element type. Values still flow through the executor as
  `*E`, and field functions may take either `*E` (no copy) or `E`.
- **Nullable input positions require a Go type that can hold null.** A
  nullable SDL input field or argument (`Int`, `Int = 1`, `[Int]`) must map
  to a pointer or slice even when a default value exists, because a client may
  still send an explicit `null`. `Omittable[V]` does not relax this: use
  `Omittable[*int]` for `Int`.
- **Leaf list shapes go two levels deep without reflection.** Scalars and
  enums register `E`, `*E`, `[]E`, `[]*E`, `[][]E` and `[][]*E`. Deeper leaf
  nesting is a schema-build error rather than a reflection fallback; only
  composite lists (`[][]*Post`) use the reflective traverser with a startup
  warning.
- **Field interceptors observe pure fields too.** When at least one
  `FieldInterceptor` is registered, every field, pure or resolver, receives a
  `FieldContext` with a materialisable path. Without field interceptors pure
  fields still run with no context allocation.
- **Introspection is a set of pure bindings** on the meta types gqlparser
  injects, installed after user options and skipped entirely when
  `DisableIntrospection()` is set. `@defer` from gqlparser's prelude is not
  advertised until it is executed.
- **`gqlhttp` returns 406** when the `Accept` header allows neither supported
  media type, and treats `*/*` and `application/*` as preferring
  `application/graphql-response+json`.
- **Typed interceptors.** `WithInterceptors(...any)` is replaced by
  `WithRequestInterceptor`, `WithOperationInterceptor` and
  `WithFieldInterceptor`, matching `grpc.UnaryInterceptor` /
  `grpc.ChainUnaryInterceptor`.
- **`Directive` / `DirectiveArgs`.** A no-argument directive uses
  `Directive(name, func(next FieldFunc) FieldFunc)`; arguments use
  `DirectiveArgs[A]`. `OBJECT` locations wrap every field of the type
  (outermost, after field directives).
- **`Query` / `Mutation` / `Subscription`** bind the schema's declared root
  type names (including `schema { query: RootQuery }`).
- **One Go type, many GraphQL objects.** `User` and `UserSummary` may share
  `*ent.User`. Abstract positions that then have more than one possible
  type require a `TypeResolver`.
- **Literal numbers are `json.Number`.** Custom scalars see the same raw
  type for `{ echo(n: 42) }` and `query($n: Big!) { echo(n: $n) }`.
- **Minimum Go version is 1.27**, so `Omittable.Or` and `Selection.Collect`
  are generic methods.
- **`Omittable` fields are unexported.** Auto bindings assign through an
  internal method; application code uses `OmittableOf` / `IsSet` / `Value` /
  `Or`.
- **`Sources` concatenates independently.** Two `SDLFS` values from different
  file systems are loaded in order; constructors do not panic.
- **DataLoader ships as `graphql-go/loader`, not in the root package.**
  `loader.New` / `loader.Loader[K, V]` / `loader.Option`. Cache and
  in-flight batches are still scoped to the `OperationContext`. Sequential
  `Load` calls in one resolver do not coalesce; use `LoadMany`.
  It was originally `graphql.NewLoader`; the move puts it behind a package
  boundary so its options cannot be confused with `ExecutorOption` — every
  `graphql.With*` is now an `ExecutorOption`, and batch options are
  `loader.WithMaxBatchSize` / `loader.WithoutCache`.
- **Wave dispatch is an exported extension point.** The executor announces a
  concurrent wave before launching sibling tasks, so batched work coalesces
  the way Facebook DataLoader dispatches at the end of a tick. The
  bookkeeping is `graphql.WaveCoordinator`, reached through
  `OperationFrom(ctx).Waves()`: `OnReady` subscribes a flush callback,
  `Park` / `Unpark` bracket a block on batched work. The loader package is
  built on it, and it is the seam for any other request-scoped batching
  extension. A nil `*WaveCoordinator` is valid and inert.
- **`OperationContext.GetOrSet`** is the atomic form of `Get` then `Set`.
  Request-scoped extensions must use it to install their per-operation
  state: sibling resolvers reach their first `Load` simultaneously, and a
  check-then-act pair lets each of them install its own state, so only the
  last write survives while the rest keep using orphaned copies. That bug
  made DataLoader degrade to N+1 nondeterministically and split the
  per-request cache; `-race` does not catch it, because `Get` and `Set` are
  individually mutex-protected.
- **Complexity, depth and query cost are Executor options**, not a separate
  `ext/complexity` package in v0.1. Over-limit operations fail before
  resolvers with `COMPLEXITY_LIMIT_EXCEEDED` or `MAX_DEPTH_EXCEEDED`.
- **`SetExtension` on `OperationContext`** copies into `Response.Extensions`
  after the operation, for Netflix/Apollo-style trace and cost metadata.

### Phase 2 Deviations

The first codegen slice is SDL-only. `examples/basic` is regenerated from
SDL; Time, `@upper` and the author DataLoader remain hand-written options
passed to `graph.NewSchema`.

- **`go/format` instead of Jennifer.** Generated files are concatenated
  strings formatted with `go/format`. Content-equal skip (not a hash) avoids
  rewriting unchanged files.
- **Groups when there are two or more group names.** The default group is
  the SDL file stem (`GroupFunc` overrides). `schema` remaps to `types` so
  it does not collide with the embed directory; `model` and the output
  package name get a `grp` suffix. A schema that resolves to a single group
  stays flat in `Output` (`NewSchema(r Resolver)`). Multiple groups emit
  `<group>/generated.go` and `type Resolvers struct { User user.Resolver; ... }`.
  One file per group, not the three of section 7.4: args, the Resolver
  interface and the bindings share a package, so splitting them multiplied the
  file count without changing what the compiler rebuilds, which is decided per
  package. A 200-entity schema went from 806 files to 404.
  Groups with only pure fields expose `Bindings()` and are omitted from
  `Resolvers`. Root fields still use `graphql.Query` / `Mutation` /
  `Subscription`, not `Object[graphql.Root]`. `model` is one package per group
  (`model/<group>/models.go`) when there are two or more groups, so a
  one-group schema edit does not invalidate every other group's compiled
  package; a single group keeps the flat `model/models.go`. Generated
  models hold only leaf fields, so object types never reference each other
  and the split is acyclic; input objects can reference across groups, and
  a cycle among them falls back to the single shared package.
- **`Manifest` and `AutoBind` are not implemented.** There is no
  `go/packages` loading.
- **`Config.Models` is `map[string]string`.** A GraphQL type maps to a Go
  type expression (`Time: time.Time`). Unmapped custom scalars become named
  `string` types in `model`. Mapped scalars are omitted from `model` and
  must still be bound with `graphql.Scalar`.
- **`NullableInputOmittable` applies to input-object fields only.**
  Field arguments stay pointers (`*int`, `*model.PostFilter`).
- **Introspection fields on roots are ignored.** gqlparser injects
  `__schema` / `__type`; they are not emitted as args or resolver methods.
- **`cmd/gqlc` reads `gqlc.yaml` with `gopkg.in/yaml.v3`.** Paths in the
  file are resolved relative to the config file's directory.

### Phase 3 Deviations

The subscription executor and `transport/gqlsse` are built; `transport/gqlws`
and APQ are not.

- **`Subscribe` returns a channel, not an iterator.** `iter.Seq2[R, error]`
  reads better but cannot be selected against `ctx.Done()` without a wrapping
  goroutine, which reintroduces the allocation and a cancellation path that
  can leak. A channel is also what the source usually already is. The
  consequence is that a source cannot report an error mid-stream: an error
  from the binding prevents the subscription from starting, and after that a
  source that can fail must carry the failure in its event type.
- **Request errors come back as `*SubscribeError`, not a `*Response`.**
  `Subscribe` returns `(<-chan *Response, error)`; on failure the channel is
  nil and the error carries the `*Response` to send, because the streaming
  protocols deliver a failed start as a protocol error rather than as a
  payload.
- **Every event is its own operation.** Each event gets a fresh
  `OperationContext`, wave coordinator and value map, and runs the whole
  operation interceptor chain including limits and cost. Sharing one context
  across events would let a DataLoader cache from the first event serve stale
  data for the life of the subscription.
- **The subscription root field is written from the event, not resolved.**
  The per-event writer substitutes the root field's executor, so field
  interceptors and field directives do not observe that one field. They still
  observe every field beneath it. Everything else — null bubbling, error
  paths, abstract types, concurrency — is the ordinary object writer.
- **`gqlsse` serves all three operation types.** Section 8.3 scopes it to
  distinct connections mode, which it is, but a query or mutation is streamed
  the same way — a single `next` followed by `complete` — so one endpoint
  covers everything and a client needs no second URL. An error raised before
  the stream opens is an ordinary HTTP error with a
  `application/graphql-response+json` body, matching `gqlhttp`, rather than a
  `next` event on an opened stream.
- **The HTTP transports share `internal/httpreq`.** Query-parameter and body
  decoding, the body limit and the CSRF check live there rather than being
  copied, because two transports that disagree about which requests are
  forgeable is a difference clients can see.

- **Subscription root fields must be bound with `Subscribe`.** `Field` or
  `Resolve` there composes cleanly and then has no stream, so it is rejected
  at `NewSchema` rather than at request time. Codegen emits
  `Subscribe`/`SubscribeArgs` and a `<-chan T` resolver signature to match.

## 12. Risks and Mitigations

| Risk | Mitigation |
|---|---|
| `gqlparser` AST memory for multi-megabyte SDL at startup | Measured in benchmarks; if significant, build a compact schema representation after `NewSchema` and drop the AST except for what introspection needs. |
| Sub-buffer splicing copies bytes | `memmove` cost is small relative to resolver work; a vectored writer can replace it later without API change. |
| Deep nested lists hit the reflection fallback | Startup warning; explicit shape registration can be added in v0.2 if real schemas need it. |
| Shared pre-decoded arguments mutated by a resolver | Documented as read-only; race detector in CI catches violations in the framework's own tests. |
| Generic instantiation count grows compile time | Instantiations are bounded by distinct `(P, R)` pairs; pointer parents share GC shapes. Tracked by the cold-build benchmark. |
| `graphql-transport-ws` and GraphQL over HTTP edge cases | Conformance tests against reference implementations. |

## 13. Glossary

- **Binding** — the association between a GraphQL type or field and Go code,
  expressed through the constructors in Section 5.
- **Group** — a set of GraphQL types whose generated bindings share one Go
  package; defaults to one group per SDL file.
- **Manifest** — a programmatic description of bindings supplied by an
  external generator; enables codegen with zero Go type loading.
- **Plan** — the compiled, cached, immutable form of an operation.
- **Pure field** — a field bound with `Field`/`FieldArgs`; no context, no
  error, executed inline.
- **Resolver field** — a field bound with `Resolve`/`ResolveArgs`; may perform
  I/O and be scheduled concurrently.
