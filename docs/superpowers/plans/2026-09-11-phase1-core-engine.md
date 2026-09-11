# Phase 1: Core and Engine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the root `graphql` package (binding API, type registry, plan compiler, executor, introspection), the `internal/jsonw` writer, the `transport/gqlhttp` handler, and a hand-written example schema that passes the execution conformance suite.

**Architecture:** Schema-first runtime: SDL is parsed by `gqlparser/v2` into `*ast.Schema`; generic binding constructors register typed adapters (writers, decoders, traversers, nil checks) in a registry keyed by `(GraphQL type, reflect.Type)`; `NewSchema` composes them into type-erased field executors and validates Go-vs-SDL shapes. Operations are compiled once into immutable plans (fragments flattened, `@skip/@include` constant-folded per variant, arguments pre-decoded, keys pre-serialized) and executed by writing JSON directly into a pooled buffer with offset-based null bubbling and semaphore-bounded concurrency.

**Tech Stack:** Go 1.24, `github.com/vektah/gqlparser/v2` v2.5.37, standard library only otherwise (`net/http`, `encoding/json`, `hash/maphash`, `log/slog`, `reflect`, `sync`).

**Spec:** `docs/superpowers/specs/2026-09-11-graphql-go-design.md`

**Execution mode:** Inline by the design author. Steps carry exact interfaces, algorithms and test expectations; routine code is specified rather than pre-written.

## Global Constraints

- Module path `github.com/syssam/graphql-go`; `go 1.24` in `go.mod`.
- Root package depends only on `gqlparser/v2` and the standard library.
- No reflection on the request hot path except the documented nested-list fallback (`[][]E`), which logs a `slog.Warn` at `NewSchema`.
- Pointer values recommended; boxing a pointer into `any` must be the only boxing on the hot path for object values.
- All code, comments and doc strings in English. No comments that narrate the code.
- Every task ends with `go vet ./...` and `go test -race ./...` passing, then a commit.
- Commit message style: imperative, lower-case type prefix (`feat:`, `test:`, `refactor:`, `docs:`).

---

## File Structure

| File | Responsibility |
|---|---|
| `go.mod`, `go.sum` | Module definition |
| `.gitignore` | Go defaults |
| `graphql.go` | Package doc, `ID`, `Root`, `Omittable[T]` |
| `internal/jsonw/writer.go` | Streaming JSON writer: buffer, marks, comma state, escaping, numbers, pool |
| `writer.go` | Public `Writer` handed to scalar marshalers (thin wrapper over `jsonw.Writer`) |
| `errors.go` | `Error`, `Location`, `Path`, `PathElem`, `ErrorPresenter`, error codes, helpers |
| `request.go` | `Request`, `Response`, envelope writing, buffer release |
| `schema.go` | `Schema`, `Source`, `NewSchema`, `SchemaOption`, builder phases (collect → resolve → validate), `PrintSDL`, `DisableIntrospection` |
| `registry.go` | Typed adapter registry: leaf writers, decoders, nil checks, traversers, `valueShape` derivation, reflection fallback |
| `binding_scalar.go` | `Scalar[T]`, built-in scalars with spec coercion |
| `binding_enum.go` | `Enum[T]` |
| `coerce.go` | Raw input value normalization (`json.Number`, AST literals), list coercion, default values |
| `binding_input.go` | `Input[T]`, `Args[A]`, `InputField`, `OmittableField`, `inputDecoder` |
| `binding_object.go` | `Object[T]`, `Field`, `FieldArgs`, `Resolve`, `ResolveArgs`, `FieldOpt`, `fieldDef` composition |
| `binding_abstract.go` | `Interface[T]`, `Union[T]`, `TypeResolver` |
| `binding_directive.go` | `Directive[A]`, `FieldFunc` |
| `validate.go` | Shape compatibility rules and `Validate` helper |
| `plan.go` | Document entry, `CollectFields`, plan compilation, skip/include variants, argument pre-decoding, complexity |
| `plan_cache.go` | LRU document cache with string-equality confirmation |
| `exec.go` | `Executor`, `ExecutorOption`, `Execute`, request pipeline, variable coercion, operation selection |
| `exec_object.go` | `writeValue`, `writeObject`, `writeList`, abstract resolution, scheduling, sub-buffer splicing |
| `exec_errors.go` | Per-request error collection, path nodes and materialization, panic recovery |
| `context.go` | `FieldContext`, `OperationContext`, `Selection`, `SelectionFrom`, `PathFrom`, `OperationFrom` |
| `interceptor.go` | `RequestInterceptor`, `OperationInterceptor`, `FieldInterceptor`, chain construction |
| `introspection.go` | `__Schema`, `__Type`, `__Field`, `__InputValue`, `__EnumValue`, `__Directive` bindings and `DisableIntrospection` rule |
| `transport/gqlhttp/handler.go` | `Handler`, options, GET/POST parsing, content negotiation, status codes, CSRF, batching |
| `examples/basic/schema/*.go`, `examples/basic/schema.graphql`, `examples/basic/main.go` | Hand-written blog schema and server |
| `README.md` | Quick start |

---

### Task 1: Module Scaffold and Primitive Types

**Files:**
- Create: `go.mod`, `.gitignore`, `graphql.go`, `graphql_test.go`

**Interfaces:**
- Produces:
  - `type ID string`
  - `type Root struct{}`
  - `type Omittable[T any] struct{ value T; set bool }` with `IsSet() bool`, `Value() T`, `ValueOK() (T, bool)`, `func OmittableOf[T any](v T) Omittable[T]`, and `MarshalJSON` (writes `null` when unset).

- [ ] **Step 1: Initialize module**

Run: `go mod init github.com/syssam/graphql-go` then `go get github.com/vektah/gqlparser/v2@v2.5.37`. Set `go 1.24` in `go.mod`.

- [ ] **Step 2: Write failing tests** (`graphql_test.go`)

Cases: zero `Omittable[*string]` → `IsSet() == false`, `Value() == nil`; `OmittableOf(ptr)` → set; `OmittableOf[*string](nil)` → set with nil value; `ValueOK` returns `(v, true)` only when set; `json.Marshal` of unset → `null`, of set string → quoted.

- [ ] **Step 3: Implement `graphql.go`** with package documentation describing the binding API in two paragraphs.

- [ ] **Step 4: Run** `go test ./...` → PASS. **Commit:** `feat: scaffold module with ID, Root and Omittable`

---

### Task 2: Streaming JSON Writer

**Files:**
- Create: `internal/jsonw/writer.go`, `internal/jsonw/writer_test.go`

**Interfaces:**
- Produces (package `jsonw`):
  - `type Writer struct{ buf []byte; stack []bool /* needComma per depth */ }`
  - `func New() *Writer`; `func Get() *Writer` / `func Put(w *Writer)` (pool, resets)
  - `func (w *Writer) Bytes() []byte`, `Len() int`, `Reset()`
  - `type Mark struct{ off, depth int; comma bool }`; `func (w *Writer) Mark() Mark`; `func (w *Writer) Rewind(m Mark)`
  - `BeginObject()`, `EndObject()`, `BeginArray()`, `EndArray()`
  - `Key(raw []byte)` — writes separator comma if needed then `raw` (which must already be `"name":`); `KeyString(name string)` — escapes and writes `"name":`
  - `Elem()` — array element separator
  - `Null()`, `Bool(bool)`, `Int64(int64)`, `Uint64(uint64)`, `Float64(float64) error` (NaN/±Inf → error), `String(string)`, `Raw([]byte)` (already-valid JSON value; handles separators like any value)
  - `func EncodeKey(name string) []byte` — returns `"name":` with escaping.
  - `func AppendString(dst []byte, s string) []byte` — exported for the public `Writer`.

Escaping: `"`, `\\`, control chars < 0x20 (`\n`, `\r`, `\t`, `\b`, `\f`, others `\u00XX`), U+2028/U+2029 as `\u2028`/`\u2029`; do not escape `<`, `>`, `&`; invalid UTF-8 bytes → `\ufffd`.

Value separators: every value-writing method calls `w.sep()` which writes `,` when `stack[top]` is true and then sets it true; `BeginObject`/`BeginArray` push `false`; `End*` pops. `Key` performs the separator step and `sep()` must then be suppressed for the following value: implement with a `pendingKey bool` flag consumed by the next value write.

- [ ] **Step 1: Write failing tests**

Cases: empty object `{}`; object with two keys uses one comma; nested arrays of objects; `Mark`/`Rewind` after writing two of three fields yields the same bytes as writing only the first field and then the third (comma state restored); `Float64(math.NaN())` returns error and writes nothing; `String` escaping table including `"a\"b"`, `"\u0001"`, `"\u2028"`, `"<&>"` unescaped, invalid byte `\xff` → `\ufffd`; `EncodeKey("na\"me")` → `"na\"me":`; pool `Get` returns zero-length writer after `Put`.

- [ ] **Step 2: Run** → FAIL (package missing).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** → PASS. Add `BenchmarkWriteObject` (100 string fields) asserting 0 allocs/op with `testing.AllocsPerRun`. **Commit:** `feat: add allocation-free streaming JSON writer`

---

### Task 3: Errors, Request and Response

**Files:**
- Create: `errors.go`, `errors_test.go`, `request.go`, `request_test.go`

**Interfaces:**
- Produces:
  - `type Location struct{ Line, Column int }`
  - `type PathElem struct{ Key string; Index int; IsIndex bool }`; `type Path []PathElem`; `func (p Path) String() string` (`user.posts[2].title`); `func (p Path) MarshalJSON() ([]byte, error)` (`["user","posts",2,"title"]`)
  - `type Error struct{ Message string; Locations []Location; Path Path; Extensions map[string]any; Err error }`; `func (e *Error) Error() string`; `func (e *Error) Unwrap() error`; `func (e *Error) MarshalJSON()` emitting keys in order `message, locations, path, extensions` and omitting empty ones.
  - `func Errorf(format string, args ...any) *Error`; `func (e *Error) WithCode(code string) *Error` (sets `extensions.code`); `func (e *Error) WithExtension(key string, v any) *Error`
  - Codes: `const CodeInternal = "INTERNAL_SERVER_ERROR"`, `CodeBadRequest = "GRAPHQL_PARSE_FAILED"`, `CodeValidation = "GRAPHQL_VALIDATION_FAILED"`, `CodeBadUserInput = "BAD_USER_INPUT"`.
  - `type ExtensionsProvider interface{ GraphQLExtensions() map[string]any }`
  - `type ErrorPresenter func(ctx context.Context, err error) *Error`; `func DefaultErrorPresenter(ctx, err) *Error`: if `errors.As(err, &*Error)` → return it (merging `ExtensionsProvider` extensions from the chain); else `&Error{Message: err.Error(), Err: err}`.
  - `type Request struct{ Query string; OperationName string; Variables json.RawMessage; Extensions map[string]any }`
  - `type Response struct{ Data []byte; Errors []*Error; Extensions map[string]any; buf *jsonw.Writer }`; `func (r *Response) WriteTo(w io.Writer) (int64, error)` writes `{"errors":[...],"data":...,"extensions":{...}}` omitting `errors` when empty, omitting `data` when `Data == nil`, omitting `extensions` when empty; `func (r *Response) MarshalJSON() ([]byte, error)`; `func (r *Response) Release()` returns the buffer to the pool and nils `Data`; `func (r *Response) HasRequestErrors() bool` (true when `Data == nil && len(Errors) > 0`).

- [ ] **Step 1: Write failing tests**

Cases: `Path.MarshalJSON` mixed keys/indices; `Error.MarshalJSON` omits empty `locations`, `path`, `extensions`; `Errorf("x").WithCode(CodeInternal)` → `extensions.code`; `DefaultErrorPresenter` unwraps `fmt.Errorf("wrap: %w", gqlErr)` to the inner `*Error`; presenter merges `GraphQLExtensions()` from a custom error type; `Response.WriteTo` for data-only, errors-only, both, with extensions; `Release` twice is a no-op.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add error, request and response types`

---

### Task 4: Schema Foundation

**Files:**
- Create: `schema.go`, `schema_test.go`

**Interfaces:**
- Produces:
  - `type Source struct{ name string; content string }`; `func SDL(s string) Source`; `func SDLBytes(b []byte) Source`; `func SDLFS(fsys fs.FS, patterns ...string) Source` (reads and concatenates matched files at `NewSchema`; each file keeps its own name for error positions; use `fs.Glob`).
  - `type SchemaOption interface{ applySchema(*schemaBuilder) }` (unexported method; implemented by all binding constructors).
  - `type Schema struct{ ast *ast.Schema; objects map[string]*objectType; abstracts map[string]*abstractType; goTypes map[reflect.Type]*objectType; reg *registry; introspection bool; directives map[string]*directiveBinding }`
  - `func NewSchema(src Source, opts ...SchemaOption) (*Schema, error)`; `func (s *Schema) AST() *ast.Schema`
  - `func PrintSDL(s *Schema) string` via `formatter.NewFormatter(&buf).FormatSchema(s.ast)`
  - `func DisableIntrospection() SchemaOption`
  - `type schemaBuilder struct{ ast *ast.Schema; reg *registry; objects map[string][]objectBinding; inputs []inputBinding; abstracts []abstractBinding; directives []directiveBinding; errs []error; introspection bool }` with `func (b *schemaBuilder) errorf(format string, args ...any)`.
  - Builder phases inside `NewSchema`: (1) load sources → `gqlparser.LoadSchema`; (2) apply options (collect only); (3) `registerBuiltins` (Task 5); (4) `resolveBindings` (Tasks 6–8, 11, 12); (5) `validateSchema` (Task 7); (6) return `errors.Join(b.errs...)` if any.
  - Introspection field injection check: confirm `gqlparser.LoadSchema` adds `__schema`/`__type` to the query type; if it does not, append the two `*ast.FieldDefinition`s in phase 1.

- [ ] **Step 1: Write failing tests**

Cases: `NewSchema(SDL("type Query { a: Int }"))` succeeds and `AST().Query.Name == "Query"` (binding validation is not active yet; this test is later tightened in Task 7 to require bindings); invalid SDL returns an error containing the gqlparser message; `SDLFS` with two files concatenates (`type Query` in one, `type A` in the other); `PrintSDL` round-trips through `NewSchema`; `DisableIntrospection()` sets `introspection == false`.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add schema loading and option plumbing`

---

### Task 5: Registry, Scalars and Enums

**Files:**
- Create: `registry.go`, `registry_test.go`, `binding_scalar.go`, `binding_scalar_test.go`, `binding_enum.go`, `binding_enum_test.go`, `writer.go`

**Interfaces:**
- Produces:
  - `type Writer = jsonw.Writer` is **not** used; instead `type Writer struct{ w *jsonw.Writer }` with methods `String`, `Int64`, `Float64() error`, `Bool`, `Null`, `Raw`. Keeping a distinct type lets the public surface stay stable while `jsonw` evolves.
  - `var errNonNull = errors.New("graphql: null value for non-null type")` and `type indexedError struct{ index int; err error }` (wraps element errors with their list index; `Unwrap`).
  - `type typeKey struct{ name string; typ reflect.Type }`
  - `type registry struct{ leafWriters map[typeKey]any; leafWritersAny map[typeKey]func(*jsonw.Writer, any, *ast.Type) error; decoders map[typeKey]any; nilChecks map[reflect.Type]func(any) bool; traversers map[reflect.Type]func(any, func(int, any) bool); argsDecoders map[reflect.Type]*inputDecoder; inputsByName map[string]*inputDecoder; leafKinds map[string]leafKind /* scalar | enum */ }`
  - `func registerLeaf[E any](r *registry, name string, write func(*jsonw.Writer, E) error, decode func(any) (E, error))` registers, for `V ∈ {E, *E, []E, []*E}`:
    - `leafWriters[key] = func(w *jsonw.Writer, v V, t *ast.Type) error` honoring `t.NonNull` at each level (nil pointer/slice → `errNonNull` if non-null else `Null()`; element errors wrapped in `indexedError`).
    - `leafWritersAny[key] = func(w, v any, t) error { return typed(w, v.(V), t) }`
    - `decoders[key] = func(raw any, t *ast.Type) (V, error)`: `nil` raw → zero for `*E`/slices, `errNonNull` for non-null; a non-list raw for a list type is coerced to a single-element list (spec list coercion); elements decoded recursively.
    - `nilChecks[TypeFor[*E]] = func(v any) bool { p, _ := v.(*E); return p == nil }`, likewise for `[]E`, `[]*E`; `nilChecks[TypeFor[E]] = nil` (never nil).
  - `func (r *registry) leafWriter(name string, t reflect.Type) (any, bool)`; `func (r *registry) decoder(name string, t reflect.Type) (any, bool)`; `func (r *registry) nilCheck(t reflect.Type) func(any) bool`
  - `func Scalar[T any](name string, marshal func(*Writer, T) error, unmarshal func(any) (T, error)) SchemaOption`
  - Built-in registrations in `registerBuiltins(b *schemaBuilder)`:
    - `Int`: Go `int`, `int32`, `int64`. Output: value must fit in int32 else error `"Int cannot represent non 32-bit signed integer value: %d"`. Input: accept `json.Number` (integral, in range), `int`, `int32`, `int64`, `float64` with zero fraction; reject strings and booleans.
    - `Float`: `float64`, `float32`. Output: non-finite → error. Input: `json.Number`, any Go numeric.
    - `String`: `string`. Input: only `string`.
    - `Boolean`: `bool`. Input: only `bool`.
    - `ID`: `ID`, `string`, `int64`, `int`. Output: `ID`/`string` as JSON string; `int64` as JSON string of the number. Input: `string` or integral `json.Number` (→ decimal string for `ID`/`string`, parsed for `int64`/`int`).
  - `func Enum[T comparable](name string, values map[T]string) SchemaOption` — validates at resolve time that every SDL enum value has a Go mapping and vice versa; output unknown Go value → error; input unknown name → error `"invalid enum value %q for %s"`.

- [ ] **Step 1: Write failing tests**

Registry: `registerLeaf` for `int` under `Int` produces writers for all four shapes; `*int` nil with `Int!` → `errNonNull`, with `Int` → `null`; `[]*int{ptr(1), nil}` with `[Int!]!` → `indexedError{index:1}`; `[]int` with `[Int]` writes `[1,2]`; decoder single-value list coercion `raw=1, t=[Int]` → `[]int{1}`; `nilCheck(TypeFor[*int])(nilPtr) == true`.

Scalars table (output): `Int` `int(2147483648)` → error; `Float` `math.Inf(1)` → error; `ID` `int64(42)` → `"42"`. Scalars table (input): `Int` from `json.Number("3")` → 3, `json.Number("3.5")` → error, `"3"` → error, `float64(3)` → 3, `json.Number("2147483648")` → error; `Float` from `json.Number("1")` → 1.0; `Boolean` from `"true"` → error; `ID` from `json.Number("7")` → `ID("7")`.

Enum: bidirectional mapping; unknown output value error; unknown input name error; enum bound with missing SDL value → `NewSchema` error listing the value.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add typed adapter registry with built-in scalars and enums`

---

### Task 6: Input Coercion and Input Bindings

**Files:**
- Create: `coerce.go`, `coerce_test.go`, `binding_input.go`, `binding_input_test.go`

**Interfaces:**
- Produces:
  - `type inputDecoder struct{ goType reflect.Type; name string; newValue func() any /* returns *T */; fields []inputFieldSetter; byName map[string]*inputFieldSetter }`
  - `type inputFieldSetter struct{ name string; def *ast.FieldDefinition | *ast.ArgumentDefinition (store as `typ *ast.Type; defaultValue *ast.Value`); set func(target any, raw any, present bool) error }`
    - `present == false` and `defaultValue != nil` → decode the default and call the setter; `present == false` and no default → do nothing (leave zero) unless `typ.NonNull` → `errNonNull`; `present == true` with `raw == nil` → nullable: call setter with typed nil (Omittable: set with nil value); non-null → error.
  - `func (d *inputDecoder) decodeObject(raw map[string]any) (any, error)` — allocates via `newValue`, iterates `fields` (not the raw map) so defaults apply; unknown keys are rejected by the validator upstream and ignored here.
  - `func (d *inputDecoder) decodeArgs(args map[string]any) (any, error)` — same as `decodeObject` but for argument structs (`present` = key exists in `args`).
  - `func Input[T any](name string, fields ...InputFieldOption) SchemaOption` — registers `inputsByName[name]` and, through `registerLeafLike`, decoders for `T`, `*T`, `[]T`, `[]*T` keyed by `(name, type)` that call `decodeObject` and convert.
  - `func Args[A any](fields ...InputFieldOption) SchemaOption` — registers `argsDecoders[TypeFor[A]]`.
  - `type InputFieldOption interface{ applyInput(*inputDecoderBuilder) }`
  - `func InputField[T, V any](name string, set func(*T, V)) InputFieldOption` — at resolve time looks up `decoders[(sdlTypeName, TypeFor[V])]`; missing → builder error `"input %s.%s: no decoder for Go type %s bound to %s"`.
  - `func OmittableField[T, V any](name string, set func(*T, Omittable[V])) InputFieldOption` — decodes `V` via the same lookup and wraps in `Omittable`, marking `set=true` whenever the key was present or a default applied.
  - `coerce.go`: `func normalizeRaw(v any) any` — converts `json.Number` recursively? No: numbers stay `json.Number`; the function only converts `[]any`/`map[string]any` trees produced by `ast.Value.Value(vars)` and by `encoding/json` into the same representation (they already match; `normalizeRaw` exists to convert `int64`/`float64` literals produced by gqlparser into `json.Number` so scalar decoders see one numeric type). `func literalValue(v *ast.Value, vars map[string]any) (any, error)` wraps `v.Value(vars)` + `normalizeRaw`. `func valueHasVariables(v *ast.Value) bool`.

- [ ] **Step 1: Write failing tests**

Cases (`type in struct{ A int; B *string; C []int; D Omittable[*string]; E *nested }`):
- required `A` missing → error; present → set.
- `B` absent → nil; present `null` → nil; present `"x"` → `"x"`.
- `C` from raw `5` → `[]int{5}` (list coercion); from `[1,2]` → `[1,2]`.
- `D` absent → `!IsSet()`; present `null` → `IsSet() && Value()==nil`; present `"x"` → set with value.
- default value from SDL (`b: String = "dflt"`) applied when absent, not applied when present `null`.
- nested input object `E` decodes recursively; `[E!]` list decodes elements.
- `Args[A]` with unknown SDL type name in setter → `NewSchema` error message contains coordinate.
- `literalValue` for `{a: 1, b: $v}` with vars → `map[string]any{"a": json.Number("1"), "b": ...}`.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add input coercion and input bindings`

---

### Task 7: Object Bindings and Shape Validation

**Files:**
- Create: `binding_object.go`, `binding_object_test.go`, `validate.go`, `validate_test.go`
- Modify: `schema.go` (resolve + validate phases), `registry.go` (traversers, `valueShape`)

**Interfaces:**
- Produces:
  - `type FieldOption interface{ applyField(*objectBindingBuilder) }`; `type FieldOpt func(*fieldSpec)`; `func Inline() FieldOpt`; `func Concurrent() FieldOpt`
  - `func Object[T any](name string, fields ...FieldOption) SchemaOption` — records `goType = TypeFor[T]`, base `E` (elem if pointer), registers `traversers[TypeFor[[]E]]` and `traversers[TypeFor[[]*E]]` (typed loops yielding `any(elem)`), `nilChecks` for `*E`, `[]E`, `[]*E`, and `goTypes[E] = goTypes[*E] = object`.
  - `func Field[P, R any](name string, fn func(P) R, opts ...FieldOpt) FieldOption`
  - `func FieldArgs[P, A, R any](name string, fn func(P, A) R, opts ...FieldOpt) FieldOption`
  - `func Resolve[P, R any](name string, fn func(context.Context, P) (R, error), opts ...FieldOpt) FieldOption`
  - `func ResolveArgs[P, A, R any](name string, fn func(context.Context, P, A) (R, error), opts ...FieldOpt) FieldOption`
  - `type fieldSpec struct{ name string; parent, args, result reflect.Type; pure bool; forceInline, forceConcurrent bool; build func(b *schemaBuilder, obj *objectType, def *ast.FieldDefinition) (*fieldDef, error) }`
  - `type fieldDef struct{ name string; def *ast.FieldDefinition; typ *ast.Type; leaf bool; writeLeaf func(ctx context.Context, w *jsonw.Writer, parent, args any) error; resolve func(ctx context.Context, parent, args any) (any, error); anyExec FieldFunc /* set when wrapped by directive/interceptor */; shape *valueShape; args *inputDecoder; pure, schedulable bool; object *objectType }`
  - `type valueShape struct{ isNil func(any) bool; traverse func(any, func(int, any) bool); elem *valueShape }`; `func (r *registry) shapeFor(t reflect.Type, sdl *ast.Type) (*valueShape, error)` — walks list levels; for each level uses `traversers[t]` if present, else `reflect`-based traversal with `slog.Warn("graphql: using reflection traversal", "type", t)` once per type; validates that the number of list levels in `t` equals the SDL list depth.
  - Composition in `build` for `Field[P,R]` on a leaf SDL type: `lw := leafWriters[(sdl.Name(), TypeFor[R])]` asserted to `func(*jsonw.Writer, R, *ast.Type) error`; `writeLeaf = func(ctx, w, parent, _ any) error { return lw(w, fn(parent.(P)), typ) }`. On a composite SDL type: `resolve = func(ctx, parent, _ any) (any, error) { return fn(parent.(P)), nil }` plus `shape`.
  - Composition for `ResolveArgs[P,A,R]`: `dec := argsDecoders[TypeFor[A]]` (missing → error `"field %s.%s: no Args[%s] registered"`); `resolve = func(ctx, parent, args any) (any, error) { return fn(ctx, parent.(P), *args.(*A)) }` (leaf variant writes via `lw` after the call).
  - `type objectType struct{ name string; def *ast.Definition; goType reflect.Type; fields map[string]*fieldDef; hasSchedulable bool }`
  - `validate.go`:
    - `func checkShape(sdl *ast.Type, goType reflect.Type, reg *registry, isInput bool) error` implementing spec §5.4 (non-null SDL with pointer Go is allowed; nullable list elements require pointer elements unless the element Go type is itself a pointer-based object; input non-null requires non-pointer, nullable requires pointer or `Omittable`).
    - Schema-level rules in `validateSchema`: every non-introspection object type in the SDL has at least one `Object` binding, and the union of bindings covers every SDL field (missing → `"type %s: field %s has no binding"`); duplicate field bindings → error; binding references an unknown type or field → error; `Object` bindings for the same name must use the same Go type; root operation types may only be bound to `Root`.
    - `func Validate(src Source, opts ...SchemaOption) error` — `NewSchema` discarding the schema.

- [ ] **Step 1: Write failing tests**

Cases: `Field` on `String!` with `func(*U) string` → OK; with `func(*U) int` → error mentioning `U.name`, `String`, `int`; `Field` on `[String!]!` with `[]string` OK, with `string` → depth mismatch error; unbound object type → error; unbound field → error; duplicate field → error; `Object[User]` and `Object[*User]` for the same type → error; `ResolveArgs` without `Args` → error; `Validate` returns nil on the fixture.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. Tighten Task 4's "unbound type succeeds" test to expect an error. **Commit:** `feat: add object bindings and schema shape validation`

---

### Task 8: Abstract Types

**Files:**
- Create: `binding_abstract.go`, `binding_abstract_test.go`
- Modify: `schema.go`

**Interfaces:**
- Produces:
  - `type AbstractOpt interface{ applyAbstract(*abstractBinding) }`; `func TypeResolver[T any](fn func(T) string) AbstractOpt`
  - `func Interface[T any](name string, opts ...AbstractOpt) SchemaOption`; `func Union[T any](name string, opts ...AbstractOpt) SchemaOption`
  - `type abstractType struct{ name string; def *ast.Definition; goType reflect.Type; resolveType func(any) string /* nil → dynamic type lookup */; possible map[string]*objectType }`
  - `func (s *Schema) concreteType(abs *abstractType, v any) (*objectType, error)`: if `resolveType != nil` → lookup `possible[name]`; else `s.goTypes[reflect.TypeOf(v)]` then confirm membership in `possible`; failure → `Errorf("abstract type %s must resolve to an object type; got %T", ...)`.
  - Validation: every possible type (from `ast.Schema.GetPossibleTypes`) must have an `Object` binding; abstract SDL types without a binding are still allowed when `T` would be `any` — so `Interface`/`Union` bindings are **optional**; unbound abstract types default to `goType = any` with dynamic lookup. Fields whose SDL type is abstract accept any Go result type.

- [ ] **Step 1: Write failing tests**

Cases: interface `Node` unbound, field returns `*User` → resolves to `User`; union `SearchResult` with `TypeResolver` override returning `"Post"`; value whose Go type is not a member → error; interface bound to Go interface `model.Node` and `Field` returning `model.Node` passes shape check.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add interface and union bindings`

---

### Task 9: Plan Compiler and Document Cache

**Files:**
- Create: `plan.go`, `plan_test.go`, `plan_cache.go`, `plan_cache_test.go`

**Interfaces:**
- Produces:
  - `type docEntry struct{ query string; doc *ast.QueryDocument; condVars []string /* sorted, ≤16 */; mu sync.Mutex; plans map[planKey]*plan }`; `type planKey struct{ op string; variant uint16 }`
  - `type planCache struct{ mu sync.Mutex; size int; items map[uint64]*list.Element; lru *list.List; seed maphash.Seed }`; `func newPlanCache(size int) *planCache`; `func (c *planCache) get(query string) (*docEntry, bool)`; `func (c *planCache) put(query string, e *docEntry)`. Zero `size` disables caching.
  - `type plan struct{ op *ast.OperationDefinition; root *objectType; sel *selectionSet; complexity int }`
  - `type selectionSet struct{ fields []*planField; byType map[string][]*planField /* abstract parents */; hasSchedulable bool }`
  - `type planFieldKind uint8` — `fieldNormal`, `fieldTypename`
  - `type planField struct{ kind planFieldKind; key []byte; alias string; def *fieldDef; astFields []*ast.Field; args any /* *A pre-decoded or nil */; dynamicArgs bool; sub *selectionSet; exec fieldExec; schedulable bool }`
  - `type fieldExec struct{ writeLeaf func(ctx, w, parent, args any) error; resolve func(ctx, parent, args any) (any, error) }` — copies of `fieldDef` functions, replaced by interceptor-wrapped versions when the executor has `FieldInterceptor`s (Task 11).
  - `func compilePlan(s *Schema, e *Executor, doc *ast.QueryDocument, op *ast.OperationDefinition, condValues map[string]bool) (*plan, error)`
  - `collectFields(parent *objectType, sels ast.SelectionSet, cond map[string]bool, visited map[string]bool) []*planField` implementing spec `CollectFields`: response-key grouping, fragment spreads/inline fragments filtered by type condition (`possibleTypes` membership), `@skip`/`@include` evaluated with literal or `condValues`, merged sub-selections; for abstract parents, `byType[name]` is computed by calling `collectFields` once per possible object type.
  - Argument pre-decoding: if no `ast.Field.Arguments` value contains a variable → `args = def.args.decodeArgs(literalArgs)` at compile time; else `dynamicArgs = true`.
  - `func condVariables(doc *ast.QueryDocument, op *ast.OperationDefinition) []string` — variables referenced by `@skip(if: $v)` / `@include(if: $v)`.
  - `func variantKey(condVars []string, vars map[string]any) uint16`.
  - Complexity: `1 + sum(children)`, lists count once (dynamic multipliers belong to `ext/complexity`).

- [ ] **Step 1: Write failing tests**

Cases: same alias in two fragments merges sub-selections into one `planField`; field on interface with inline fragments produces `byType` for each implementor with the shared fields first; `@include(if: false)` literal removes the field; `@skip(if: $s)` with `condValues{s:true}` removes the field and `condVariables` returns `["s"]`; nested fragment spread cycle guard (validator rejects cycles; test only that `visited` prevents duplicate expansion of the same spread at the same level); pre-decoded args for literal-only arguments, `dynamicArgs` when a variable appears nested inside an input object literal; `key` bytes equal `jsonw.EncodeKey(alias)`; cache: hit after put, LRU eviction at size 2, string mismatch on forced hash collision (inject `seed` and a stub hash function via an unexported package variable) → miss.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add plan compiler with skip/include variants and document cache`

---

### Task 10: Executor

**Files:**
- Create: `exec.go`, `exec_object.go`, `exec_errors.go`, `exec_test.go`, `exec_conformance_test.go`

**Interfaces:**
- Produces:
  - `type Executor struct{ schema *Schema; cache *planCache; sem chan struct{}; presenter ErrorPresenter; recover bool; reqChain RequestHandler; opChain OperationHandler; fieldInterceptors []FieldInterceptor }`
  - `type ExecutorOption func(*Executor)`; `func WithMaxConcurrency(n int)`, `WithPlanCache(size int)`, `WithErrorPresenter(p ErrorPresenter)`, `WithRecover(enabled bool)`, `WithInterceptors(is ...any)` (accepts values implementing any interceptor interface; panics on unknown).
  - `func NewExecutor(s *Schema, opts ...ExecutorOption) *Executor` — defaults: `MaxConcurrency = runtime.GOMAXPROCS(0) * 4`, cache 1024, `DefaultErrorPresenter`, recover on.
  - `func (e *Executor) Execute(ctx context.Context, req *Request) *Response`
  - Pipeline in `execute(ctx, req)`: cache lookup → `gqlparser.LoadQuery` on miss (errors → request error response with `CodeBadRequest`/`CodeValidation`); operation selection by name (`doc.Operations.ForName`; ambiguity/missing → error `"operation %q not found"` / `"must provide operation name"`); variables: `json.Unmarshal` with `UseNumber` into `map[string]any`, then `validator.VariableValues(schema, op, vars)` (error → `CodeBadUserInput`, request error); variant key → plan (compile under `docEntry.mu`); build `OperationContext`; run op chain → `runOperation`.
  - `runOperation`: `w := jsonw.Get()`; `st := &execState{...}`; `ok := st.writeObject(ctx, w, plan.root, plan.sel, Root{}, nil, plan.op.Operation == ast.Mutation)`; if `!ok` → `Data = []byte("null")`; else `Data = w.Bytes()`; attach `w` to `Response` for `Release`.
  - `type execState struct{ e *Executor; vars map[string]any; mu sync.Mutex; errs []*Error; ctx context.Context }`
  - `type pathNode struct{ parent *pathNode; key string; index int; isIndex bool }`; `func (n *pathNode) materialize() Path`
  - `exec_object.go`:
    - `func (st *execState) writeObject(ctx, w *jsonw.Writer, obj *objectType, sel *selectionSet, val any, path *pathNode, serial bool) (ok bool)`: choose `fields = sel.fields` or `sel.byType[obj.name]`; mark; `BeginObject`; if `!serial && sel.hasSchedulable && countSchedulable(fields) >= 2` → **concurrent path** else **inline path**; `EndObject`; return.
    - Inline path per field: `fm := w.Mark(); w.Key(f.key)`; execute (`fieldTypename` → `w.String(obj.name)`; leaf → `writeLeaf`; composite → `resolve` then `writeValue`); on failure: if `f.def.typ.NonNull` → `w.Rewind(mark)`, return `false`; else `w.Rewind(fm); w.Key(f.key); w.Null()`.
    - Concurrent path: for each schedulable field try `select { case st.e.sem <- struct{}{}: go task(i) default: task(i) inline }`; each task gets `sub := jsonw.Get()`, writes only the value (no key), stores `results[i] = taskResult{buf: sub, ok: bool}`; `wg.Wait()`; then iterate fields in order writing pure fields inline and splicing `w.Key(f.key); w.Raw(sub.Bytes())` for tasks (with the same null-bubbling rules; `jsonw.Put(sub)` after splice).
    - `func (st *execState) writeValue(ctx, w, v any, t *ast.Type, shape *valueShape, sub *selectionSet, path *pathNode) bool`: nil check (`shape.isNil != nil && shape.isNil(v)`) → `Null()` or `false` if `t.NonNull`; list (`t.Elem != nil`) → `writeList`; abstract named type → `concreteType`; error → record, return `!t.NonNull` after writing `Null()`; object → `writeObject(child)`.
    - `func (st *execState) writeList(...)`: mark; `BeginArray`; `shape.traverse(v, func(i, e) bool { em := w.Mark(); w.Elem(); ok := writeValue(e, t.Elem, shape.elem, sub, &pathNode{path, index:i}); if !ok { if t.Elem.NonNull { failed = true; return false }; w.Rewind(em); w.Elem(); w.Null() }; return true })`; if `failed` → `w.Rewind(mark)`; return `false`; else `EndArray`; return `true`. When `sub != nil && sub.hasSchedulable && len >= 2` (length via a first pass counting through `traverse`) → elements execute concurrently into sub-buffers using the same semaphore pattern, then splice.
    - `func (st *execState) callResolve(ctx, f *planField, parent, args any, path *pathNode) (v any, err error)`: computes `args` for `dynamicArgs` via `f.def.args.decodeArgs(f.astFields[0].ArgumentMap(st.vars))`; wraps `ctx` with `FieldContext` (Task 11; until then plain ctx); `defer` recover when `e.recover` → `Errorf("internal system error").WithCode(CodeInternal)` and `slog.Error("graphql: resolver panic", "path", ..., "panic", r, "stack", debug.Stack())`.
    - `exec_errors.go`: `func (st *execState) addError(err error, path *pathNode, pos *ast.Position)`: presented := `e.presenter(ctx, err)`; set `Path` if empty; append `Location` from `pos`; lock; append.
  - Context cancellation: before each field, `if ctx.Err() != nil` → record once (`CodeInternal` is wrong; use message `ctx.Err().Error()` with code `"REQUEST_CANCELLED"`) and stop writing further fields (treat remaining as nullable nulls; non-null → bubble).

- [ ] **Step 1: Write failing tests** (`exec_conformance_test.go` uses a fixture schema defined in `fixture_test.go` with `User{ID, Name, Nick *string, Tags []string, Friends []*User, Pet Pet(interface) }`, `Query{ me, user(id), users, search(term): [SearchResult!]!, fail: String!, failNullable: String, list: [String!]!, panics: Int }`, `Mutation{ inc: Int!, log(msg: String!): [String!]! }`)

Each case: `(query, variables) → expected JSON` compared after canonicalizing with `encoding/json`:
1. scalar and alias: `{ me { id n: name } }`.
2. nullable null: `{ me { nick } }` → `{"me":{"nick":null}}`.
3. list of scalars and list of objects with nested selection.
4. non-null field error bubbles to nearest nullable parent: `{ me { fail } }` → `{"me":null}` with error path `["me","fail"]`.
5. nullable field error: `{ me { failNullable } }` → data with `null`, one error.
6. list element non-null failure nulls the list; `[String]!` with nil element writes `null` element.
7. interface with inline fragments and `__typename`.
8. union `search` with `TypeResolver`.
9. variables with default values; missing required variable → request error, `Data == nil`.
10. `@include(if: $v)` variants: `true` and `false` produce different plans, both cached under one `docEntry`.
11. mutation serial: `mutation { a: inc b: inc c: inc }` yields `1,2,3` in order (resolver increments a counter with a `time.Sleep(2ms)` to expose reordering).
12. query concurrency: two resolvers each sleeping 30ms complete in under 50ms; with `WithMaxConcurrency(1)` still completes correctly (inline fallback) and takes ≥ 60ms.
13. panic recovery → `INTERNAL_SERVER_ERROR`, message masked, data `null` for non-null field.
14. `WithRecover(false)` → panic propagates (assert with `recover` in test).
15. unknown operation name → error; two anonymous operations → validation error from gqlparser.
16. cancelled context → response contains `REQUEST_CANCELLED` error.
17. `__typename` at root → `"Query"`.
18. introspection placeholder skipped here (Task 12).
19. `Response.Release()` then `Execute` again reuses buffers without data races (`-race`).
20. error `Extensions` from `ExtensionsProvider` flow through; custom `ErrorPresenter` masks messages.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (`exec.go`, `exec_object.go`, `exec_errors.go`). **Step 4: Run** `go test -race ./...` → PASS. Add `BenchmarkExecuteShallowList` (100 users × 3 scalar fields) and record allocs/op in the commit message. **Commit:** `feat: add executor with null bubbling and bounded concurrency`

---

### Task 11: Context Helpers, Interceptors and Directives

**Files:**
- Create: `context.go`, `context_test.go`, `interceptor.go`, `interceptor_test.go`, `binding_directive.go`, `binding_directive_test.go`
- Modify: `exec.go`, `exec_object.go`, `plan.go`, `schema.go`

**Interfaces:**
- Produces:
  - `type OperationContext struct{ Operation *ast.OperationDefinition; Doc *ast.QueryDocument; RawQuery string; Variables map[string]any; Stats struct{ ParseStart, ExecStart time.Time; CacheHit bool }; values sync.Map }` with `Set(key, v any)`, `Get(key any) (any, bool)`; `func OperationFrom(ctx) *OperationContext`.
  - `type FieldContext struct{ Field *ast.FieldDefinition; Object *ast.Definition; Args any; Parent any; field *planField; path *pathNode }`; `func FieldFrom(ctx) *FieldContext`; `func PathFrom(ctx) Path` (materializes `path`); `func SelectionFrom(ctx) Selection`.
  - `type Selection struct{ set *selectionSet; typeName string }`; `func (s Selection) Has(name string) bool`; `func (s Selection) Fields() iter.Seq[SelectedField]`; `func (s Selection) Sub(name string) (Selection, bool)`; `type SelectedField struct{ Name, Alias string; Args any /* pre-decoded or nil when dynamic */ }`. For abstract parents `Fields()` iterates the union across `byType` entries de-duplicated by alias.
  - `type RequestHandler func(ctx context.Context, req *Request) *Response`; `type OperationHandler func(ctx context.Context, op *OperationContext) *Response`; `type FieldHandler func(ctx context.Context) (any, error)`
  - `type RequestInterceptor interface{ InterceptRequest(ctx, *Request, next RequestHandler) *Response }`; `type OperationInterceptor interface{ InterceptOperation(ctx, *OperationContext, next OperationHandler) *Response }`; `type FieldInterceptor interface{ InterceptField(ctx, *FieldContext, next FieldHandler) (any, error) }`
  - Chain construction in `NewExecutor`: `reqChain = fold(interceptors, e.execute)`; `opChain = fold(opInterceptors, e.runOperation)`. Field interceptors: in `compilePlan`, if `len(e.fieldInterceptors) > 0` every `planField.exec` is replaced with an `any`-path wrapper: `resolve = func(ctx, parent, args) { fc := ...; return chain(ctx, fc, func(ctx) (any, error) { return original(ctx, parent, args) }) }`; for leaf fields the wrapper's result is written through `leafWritersAny`.
  - `type FieldFunc func(ctx context.Context, parent any, args any) (any, error)`
  - `func Directive[A any](name string, fn func(next FieldFunc, args A) FieldFunc) SchemaOption` — resolve phase: for every SDL field carrying `@name(...)`, decode the directive arguments into `A` (via `Args[A]`-style decoder built from the directive definition's arguments using the same `inputDecoder` machinery), and replace the field's executors with the wrapped `any`-path form (`anyExec`). Directive locations supported in v0.1: `FIELD_DEFINITION` (others ignored with a `slog.Warn`).
  - `callResolve` now sets `ctx = context.WithValue(ctx, fieldCtxKey{}, fc)` for resolver fields only (pure fields never allocate a context).

- [ ] **Step 1: Write failing tests**

Cases: `SelectionFrom` inside `users` resolver reports `Has("friends") == true` and `Sub("friends").Has("name")`; `PathFrom` inside nested resolver → `users[1].friends`; request interceptor order (outer before inner, recorded in a slice); operation interceptor sees `CacheHit == false` then `true`; field interceptor counts invocations equal to resolver fields (pure fields excluded); `@upper` directive on `name: String! @upper` upper-cases the output; directive with args `@prefix(with: "Dr. ")`.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add field/operation context, interceptors and schema directives`

---

### Task 12: Introspection

**Files:**
- Create: `introspection.go`, `introspection_test.go`
- Modify: `schema.go` (register bindings in resolve phase; validation rule), `exec.go` (`LoadQueryWithRules` with the introspection rule when disabled)

**Interfaces:**
- Produces (all unexported):
  - `type introType struct{ def *ast.Definition; ref *ast.Type }`; `func typeRef(t *ast.Type, s *ast.Schema) *introType`
  - `type introInputValue struct{ name, description string; typ *ast.Type; defaultValue *ast.Value; directives ast.DirectiveList }` built from `*ast.ArgumentDefinition` and input `*ast.FieldDefinition`.
  - `func introspectionOptions(s *ast.Schema) []SchemaOption` returning `Object[*ast.Schema]("__Schema", ...)`, `Object[*introType]("__Type", ...)`, `Object[*ast.FieldDefinition]("__Field", ...)`, `Object[*introInputValue]("__InputValue", ...)`, `Object[*ast.EnumValueDefinition]("__EnumValue", ...)`, `Object[*ast.DirectiveDefinition]("__Directive", ...)`, `Enum[ast.DefinitionKind]("__TypeKind", ...)` plus `LIST`/`NON_NULL` handled through `introType`, `Enum[ast.DirectiveLocation]("__DirectiveLocation", ...)`, and root fields `Object[Root]("Query", ResolveArgs("__type", ...), Resolve("__schema", ...))`.
  - `__Type.fields(includeDeprecated: Boolean = false)`, `enumValues(includeDeprecated)`, `__Field.args(includeDeprecated)`, `__Type.inputFields(includeDeprecated)`; `isDeprecated`/`deprecationReason` from `@deprecated`; `specifiedByURL` from `@specifiedBy(url:)`; `__Directive.isRepeatable`; `__Schema.description`.
  - `__Type.kind` mapping: `ast.Scalar→SCALAR`, `Object→OBJECT`, `Interface→INTERFACE`, `Union→UNION`, `Enum→ENUM`, `InputObject→INPUT_OBJECT`, and `LIST`/`NON_NULL` from `ref`.
  - `__Schema.types` sorted by name including built-in scalars and introspection types.
  - `var noIntrospectionRule = rules.Rule{Name: "NoIntrospection", RuleFunc: ...}` rejecting `__schema`/`__type` with message `"GraphQL introspection is not allowed"`; applied through `gqlparser.LoadQueryWithRules` when `schema.introspection == false`.

- [ ] **Step 1: Write failing tests**

Cases: full standard introspection query (the one used by GraphiQL) executes without errors and includes `Query`, `User`, `__Schema`; `{ __type(name:"User") { fields { name type { kind name ofType { kind name } } } } }` shows `[String!]!` as `NON_NULL → LIST → NON_NULL → SCALAR String`; deprecated field hidden by default and shown with `includeDeprecated: true` with reason; custom scalar `specifiedByURL`; repeatable directive `isRepeatable == true`; `DisableIntrospection()` → validation error with the exact message; `__typename` still allowed when introspection is disabled.

- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add spec-complete introspection`

---

### Task 13: HTTP Transport

**Files:**
- Create: `transport/gqlhttp/handler.go`, `transport/gqlhttp/handler_test.go`

**Interfaces:**
- Produces (package `gqlhttp`):
  - `type Handler struct{ exec *graphql.Executor; maxBody int64; batchMax int; csrfHeaders []string; csrf bool; logger *slog.Logger }` implementing `http.Handler`.
  - `func New(exec *graphql.Executor, opts ...Option) *Handler`; `type Option func(*Handler)`; `WithMaxBodyBytes(n int64)` (default 1 MiB), `WithBatching(max int)` (default disabled), `WithCSRFPrevention(enabled bool, headers ...string)` (default enabled, headers `GraphQL-Require-Preflight`, `X-Requested-With`), `WithLogger(*slog.Logger)`.
  - Media types: `const MediaTypeGraphQLResponse = "application/graphql-response+json"`, `MediaTypeJSON = "application/json"`.
  - Behaviour matrix:
    | Case | Status | Body |
    |---|---|---|
    | Method not GET/POST | 405, `Allow: GET, POST` | JSON error |
    | GET without CSRF header (when enabled) | 403 | JSON error |
    | GET with `mutation` operation | 405 | JSON error `"mutations are not allowed over GET"` |
    | POST with unsupported `Content-Type` | 415 | JSON error |
    | Body too large | 413 | JSON error |
    | Malformed JSON / missing `query` | 400 | GraphQL `errors` |
    | Batch body when batching disabled | 400 | JSON error |
    | Request errors (parse/validation/variables) and `Accept` prefers `application/graphql-response+json` (or `Accept` absent / `*/*`) | 400 | GraphQL response |
    | Request errors and `Accept: application/json` only | 200 | GraphQL response |
    | Execution result (with or without field errors) | 200 | GraphQL response |
  - GET parameters: `query`, `operationName`, `variables` (JSON object string), `extensions` (JSON object string). Detecting a mutation over GET requires the parsed document: `Executor` exposes `func (e *Executor) OperationKind(query, opName string) (ast.Operation, error)` using the document cache (add to `exec.go`).
  - Response headers: `Content-Type: <negotiated>; charset=utf-8`; body via `Response.WriteTo`; `Release` after write. Batch responses are a JSON array in request order, executed sequentially.

- [ ] **Step 1: Write failing tests** covering each row of the matrix with `httptest`, plus a GET happy path with the CSRF header and URL-encoded variables, and a batch of two queries.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Commit:** `feat: add GraphQL over HTTP transport`

---

### Task 14: Example Schema, README and Phase Gate

**Files:**
- Create: `examples/basic/schema.graphql`, `examples/basic/schema/model.go`, `examples/basic/schema/bindings.go`, `examples/basic/schema/resolvers.go`, `examples/basic/schema/schema_test.go`, `examples/basic/main.go`, `README.md`
- Modify: `docs/superpowers/specs/2026-09-11-graphql-go-design.md` only if implementation forced a deviation (record it in a "Deviations" subsection).

**Interfaces:**
- Example schema: `Node` interface; `User implements Node { id, name, email, role: Role!, posts(first: Int = 10): [Post!]!, createdAt: Time! }`; `Post implements Node { id, title, body, author: User!, tags: [String!]!, publishedAt: Time }`; `union SearchResult = User | Post`; `enum Role { ADMIN USER }`; `scalar Time @specifiedBy(url: "https://tools.ietf.org/html/rfc3339")`; `input PostFilter { authorId: ID, tag: String }`; `input UpdatePostInput { title: String, body: String }` (both fields `OmittableField`); `Query { node(id: ID!): Node, user(id: ID!): User, users: [User!]!, posts(filter: PostFilter): [Post!]!, search(term: String!): [SearchResult!]! }`; `Mutation { createPost(authorId: ID!, title: String!, body: String!): Post!, updatePost(id: ID!, input: UpdatePostInput!): Post! }`; directive `@upper on FIELD_DEFINITION` applied to `User.name`.
- `schema.NewSchema(store *Store) (*graphql.Schema, error)`; `main.go` serves `/graphql` with `gqlhttp.New(graphql.NewExecutor(s))` on `:8080`, graceful shutdown via `signal.NotifyContext`.
- `schema_test.go`: executes eight representative operations (node lookup through interface, nested posts with `first`, filter input, union search with `__typename`, `updatePost` distinguishing absent vs `null` body, `@upper`, introspection sanity, non-null error bubbling) and asserts canonical JSON.

- [ ] **Step 1: Write the example and its tests.** **Step 2: Run** `go vet ./... && go test -race -count=1 ./...` → PASS.
- [ ] **Step 3: README** — badges placeholder omitted; sections: Why, Install, Quick start (20-line hand-written example), Status (Phase 1 of the spec), Roadmap link to the spec.
- [ ] **Step 4: Commit:** `feat: add basic example, README and phase 1 gate`

---

## Self-Review

- **Spec coverage:** §5.1–5.7 → Tasks 1, 5–8, 11; §6.1–6.7 → Tasks 9, 10, 12; §8.1 (without APQ) → Task 13; §8.4 → Tasks 4, 10; §10.1 unit/conformance/fuzz → Tasks 2, 10 (fuzz for `jsonw` and coercion is added in Task 14's gate as `FuzzString` and `FuzzDecodeInt`); §11 phase 1 exit criterion → Task 14. Not in this plan by design: §7 codegen, §8.2–8.3, §8.5–8.6, §9 v0.2 items, §10.2 benchmarks.
- **Placeholder scan:** none; every step names files, signatures, cases and expected outcomes.
- **Type consistency:** `fieldDef.writeLeaf/resolve` (Task 7) are copied into `planField.exec` (Task 9) and wrapped in Task 11; `inputDecoder.decodeArgs` (Task 6) is used by Task 9 pre-decoding and Task 10 `callResolve`; `jsonw.Mark/Rewind/Key/Elem/Raw` (Task 2) are the only writer primitives the executor (Task 10) relies on; `Response.WriteTo/Release` (Task 3) are consumed by Task 13.
