# Field observation without type erasure

- **Date:** 2026-09-16
- **Module:** `github.com/syssam/graphql-go`
- **Status:** Implemented on branch `field-observer`
- **Files:** `interceptor.go`, `exec_object.go`, `context.go`, `ext/otel/otel.go`

## 1. What this costs today, measured

`CLAUDE.md` says field spans "cost more than they look". They cost four times
what the request costs without them. Measured on `{ users { id name nick tags
role } }` — the shape `BenchmarkExecuteUsers` uses — with a **no-op** field
interceptor registered, `-count=12`:

| configuration | allocs/op | B/op | ns/op |
|---|---:|---:|---:|
| no interceptor | 19 | 1025 | ~1.2 µs |
| interceptor registered | **105** | 5013 | ~5.65 µs |

+453% allocations and +302% time for an interceptor that does nothing. The
allocation figures carry ±0% variance across samples, so they are not a
property of a loaded machine.

`-count=12` is not interleaving, although this section first called it that:
`go test -bench` runs every count of one benchmark before starting the next,
so the two configurations were measured back to back, not alternated. The
allocation figures are unaffected, because allocations are deterministic. The
timings are batched and should be read as such; interleaving means building the
test binary once and alternating separate invocations of it (or of two
binaries). A later batched pair on this machine read +40% where alternated runs
of the same pair read about +16%.

### 1.1 Where the 86 extra allocations go

A third measurement isolates the two halves. Patching `interceptedExec`'s leaf
branch to keep the typed writer while still building and attaching the
`FieldContext` gives 66 allocs/op. So:

| source | allocs |
|---|---:|
| `FieldContext` machinery | 47 |
| type erasure (`anyResolve` + `writeAny`) | 39 |

The `FieldContext` half is three allocations per field, all in
`execState.fieldContext` (`exec_object.go:105`): the `&FieldContext{}`, the
`&pathNode{}` built for it, and the `context.WithValue` that attaches it.

### 1.2 Why the type-erased half is pure waste for an observer

`interceptedExec` replaces a leaf's typed `fd.writeLeaf` with a chain through
`fd.anyResolve` and `writeAny`. That exists so an interceptor can see and
replace the field's value. `ext/otel`'s `interceptField` — the only field
interceptor in this repository — never reads `v`; it returns `v, err`
unchanged. It uses the object name, the field name, the path, and the error.

Every observer has that shape. Observation does not need the value, and paying
for a type-erased round trip to hand an observer something it discards is the
whole of the 39.

### 1.3 Why part of the `FieldContext` half is also waste

`interceptedExec` obtains its `FieldContext` with `FieldFrom(ctx)` — but
`InterceptField(ctx, fc, next)` already takes it as a parameter. The value goes
through the context only because `fieldExec.writeLeaf`'s signature cannot carry
it, so `callLeaf` stores it in the context and `interceptedExec` reads it back.

`FieldFrom` and `PathFrom` are public, so the attachment cannot simply be
dropped: a resolver may read either. But `Field`/`FieldArgs` accessors take no
context at all, so for a **pure** field the attachment serves nothing but the
interceptor — which already has the value as an argument.

## 2. Goals and Non-Goals

**Goals**

1. Let an extension observe every field — including pure ones — without routing
   any field through the type-erased path.
2. Keep `ext/otel`'s field spans nesting as they do today: a field's span is the
   parent of any span its resolver starts. It is not the parent of its
   children's spans, and was not under the interceptor either: the resolver
   returns and the span ends before the sub-selection is written, and the
   sub-selection runs under the context the field itself received. Nesting
   children would change what a composite field's span duration means, and
   needs its own design.
3. Make the existing `FieldInterceptor` cheaper as a side effect, without
   changing its contract for interceptors that use their `fc` parameter.
4. Land a benchmark first, so every later claim in this design has an
   instrument. The repository currently has no benchmark that exercises the
   field interceptor path at all.

**Non-Goals**

- Removing `FieldInterceptor`. An interceptor that replaces a field's value is a
  legitimate thing to want and nothing else offers it.
- Making observation free. An observer that starts a span allocates a span; that
  is its own cost. This design is about what the engine spends before the
  observer does anything.
- Changing `FieldFrom`/`PathFrom` for resolver fields.

## 3. The observer interface

```go
// FieldObserver watches every field without seeing its value. Unlike a
// FieldInterceptor it cannot change the result, which is what lets the engine
// keep each field on its typed write path.
type FieldObserver interface {
	// BeginField runs before the field. The returned context is the one the
	// field's own resolver runs under, so a span the resolver starts nests
	// beneath the observer's. Children do not run under it.
	BeginField(ctx context.Context, f FieldInfo) context.Context

	// EndField runs when the resolver returns, before a composite field's
	// sub-selection is written, with the context this observer's BeginField
	// returned.
	EndField(ctx context.Context, f FieldInfo, err error)
}
```

`EndField` receives the context `BeginField` returned, so an observer that put
state in that context reads it back out rather than storing it anywhere. For
OpenTelemetry this is exactly `tracer.Start`'s own idiom: `Start` returns a
context carrying the span, and `trace.SpanFromContext(ctx).End()` finishes it.

### 3.1 Why not a closure

An earlier draft had `BeginField` return `(context.Context, func(error))`. It
was rejected: the closure is an allocation per field that the engine imposes on
every observer, and it buys nothing, because the state an observer wants to
carry is already in the context it returned. The two-method form costs the
machinery no allocations — no closure, no `any` boxing, no map. It is not free
in time: two interface calls and a defer per observed field. A reviewer's
alternated runs on a loaded machine put that at roughly +16% on the benchmark
query with a no-op observer; treat the figure as indicative, not settled.

### 3.2 `FieldInfo` is a value

```go
type FieldInfo struct {
	Object string
	Field  string
	Alias  string

	pathParent *pathNode
}

func (f FieldInfo) Path() Path
```

A struct passed by value, holding the parent path node and the alias, with the
response path materialized only if `Path()` is called. Nothing here allocates on the way to an observer that does not ask
for the path. It deliberately carries no `Args` and no `Parent`: those are what
a `FieldContext` is for, and an observer that needs them wants an interceptor.

## 4. Making the existing interceptor cheaper

Two changes, neither touching the public interceptor contract:

1. **Pass the `FieldContext` rather than attaching it.** `fieldExec`'s function
   types gain a `*FieldContext` parameter, `callLeaf` passes the one it already
   builds, and `interceptedExec` stops calling `FieldFrom(ctx)`. The context
   attachment then happens only where a resolver might read it — that is, for
   non-pure fields, which is what happens today when no interceptor is
   registered.
2. **Build the path node lazily.** `FieldContext` keeps the parent `*pathNode`
   and the alias, and materializes `&pathNode{}` when `Path()` is called.

**Observable change, and it is a real one:** an interceptor that ignores its
`fc` parameter and calls `FieldFrom(ctx)` instead currently works on pure
fields and will stop, because the context no longer carries a `FieldContext`
there. `PathFrom` and `SelectionFrom` read the same attachment and break the
same way: inside a pure field they return nil and an empty `Selection`. The parameter has always been the documented way in and carries the same
value. This is a pre-1.0 break, taken deliberately, and it is the reason this
change is in the spec rather than being called an optimization.

## 5. Wiring

`WithFieldObserver(o ...FieldObserver) ExecutorOption`, chained outermost-first
like the other interceptor options.

The executor calls observers around a field's execution in `callLeaf` and in
the composite path. It keeps the context each observer's `BeginField` returned
and hands that one back to the same observer's `EndField` — not the innermost
context, which with two observers would give the outer one the inner one's
span. Up to four contexts are kept in a stack array, so the common case
allocates nothing. When no observer is registered, neither call site does anything the
current code does not already do.

`ext/otel`'s `WithFieldSpans` switches from `WithFieldInterceptor` to
`WithFieldObserver`. Its `interceptField` becomes a `BeginField`/`EndField`
pair: `Start` in the first, `RecordError`/`SetStatus`/`End` in the second. No
behaviour changes — the same span name, the same two attributes, the same error
recording — and field spans stop forcing every field onto the type-erased path.

## 6. Testing

`sh scripts/gate.sh` is the gate.

| Test | Assertion |
|---|---|
| **Benchmark, landed first** | `BenchmarkFieldPathBare` / `WithInterceptor` / `WithObserver` on one fixture. This is the instrument; it must exist before either change so both can be measured against it, and because nothing in the repository benchmarks this path today |
| Observer sees every field | Including pure ones, in selection order, with correct object/field/alias |
| Observer context | `EndField` receives the context `BeginField` returned; a value put in by `BeginField` is readable in `EndField`. With several observers sharing a key, each `EndField` reads its own observer's value. A child field's `BeginField` does **not** see it (goal 2), and no test claims otherwise |
| Typed path preserved | With an observer registered, a leaf must still use `fd.writeLeaf`. **Assert this structurally, not by timing** — inspect the compiled `fieldExec`, or count allocations against a bound. A timing assertion cannot distinguish the two paths reliably on this machine |
| Interceptor still works | Existing field interceptor tests pass unchanged, including one that replaces a value |
| `FieldFrom` contract | Still returns a `FieldContext` inside a resolver field; a test pins that it returns nil inside a pure accessor, which is already true without interceptors |
| otel parity | Field spans have the same names, attributes, nesting and error recording as before the change |
| **Deliberate break** | Remove the observer's typed-path shortcut so observers route through `anyResolve`; the allocation-bound test must fail |

## 7. Expected result

The machinery cost of observing a field should fall from 86 extra allocations to
near zero: no type erasure, no `context.WithValue`, no `FieldContext`, no path
node unless asked for. What remains is whatever the observer itself allocates.

This is a prediction, not a measurement. The benchmark from section 6 lands
first precisely so that it can be checked rather than asserted — the same
discipline that turned a reported +8.66% regression on the previous branch into
a measured −1.23%, and that produced the numbers in section 1 rather than the
adjective in `CLAUDE.md`.
