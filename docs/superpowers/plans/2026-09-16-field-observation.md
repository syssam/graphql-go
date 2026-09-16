# Field Observation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an extension observe every field, pure ones included, without routing any field through the type-erased path, and make the existing field interceptor cheaper on the way.

**Architecture:** `fieldExec`'s two function types gain a `*FieldContext` parameter, so `callLeaf`/`callResolve` hand the interceptor the value it currently fishes back out of the context. The context attachment then happens only for resolver fields, where `FieldFrom`/`PathFrom` are public guarantees. A new `FieldObserver` — two methods, no value, no closure — hooks the same two choke points and leaves each field on its typed writer.

**Tech Stack:** Go 1.27, `github.com/vektah/gqlparser/v2`, `go.opentelemetry.io/otel` (in `ext/otel` only). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-16-field-observation-design.md`

## Global Constraints

- **Root package may depend only on `gqlparser/v2` and the standard library.**
- **No reflection on the request hot path.**
- **No `unsafe`, no `uintptr`.**
- **Go 1.27 minimum.**
- **Adding a field to `execState` or `OperationContext` is a hot-path change** — both sit on a size-class boundary. Check with `unsafe.Sizeof` and `benchstat` before growing either.
- **Comments explain why, not what. English only. No code-narrating comments.**
- **Tests live beside the code in `package graphql`.**
- **Commit messages: imperative, lower-case type prefix.**
- **The gate is `sh scripts/gate.sh`** — this repository has four modules and `go test ./...` reaches one.
- **`-race` is not optional.**
- **Benchmark comparisons are interleaved, never two sequential sets.** Build two test binaries and alternate them, or put both configurations in one binary and let `-count` alternate them. A sequential comparison on this machine reported +8.66% for a change measured at −1.23% interleaved, and separately reported 50% slower for a change that does strictly less work.
- **Undo a deliberate break with a reverse edit, not `git checkout -- <file>`** — on a file whose real change is not yet committed that reverts to HEAD and destroys the work.

---

### Task 1: Benchmark the field interceptor path

Nothing in this repository benchmarks the field interceptor path. Every claim the rest of this plan makes is measured against this task's output, so it lands first and alone.

**Files:**
- Create: `bench_fieldpath_test.go`

**Interfaces:**
- Consumes: `newFixture()`, `fixtureSDL`, `f.options()` from `fixture_test.go`.
- Produces, used by Task 3: `benchFieldPath(b *testing.B, opts ...ExecutorOption)`.

- [ ] **Step 1: Write the benchmarks**

```go
package graphql

import (
	"context"
	"testing"
)

// benchFieldPath runs the query BenchmarkExecuteUsers uses -- a list whose
// elements carry five pure leaf fields -- under whatever executor options are
// given, so the cost of observing a field can be read against the cost of not
// observing one.
func benchFieldPath(b *testing.B, opts ...ExecutorOption) {
	b.Helper()
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), f.options()...)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s, opts...)
	req := &Request{Query: `{ users { id name nick tags role } }`}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(ctx, req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

func BenchmarkFieldPathBare(b *testing.B) { benchFieldPath(b) }

// BenchmarkFieldPathInterceptor uses a no-op interceptor deliberately: it
// measures what the machinery costs before an observer does any work of its
// own.
func BenchmarkFieldPathInterceptor(b *testing.B) {
	benchFieldPath(b, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			return next(ctx)
		})))
}
```

- [ ] **Step 2: Run them and record the numbers**

Run: `go test -run '^$' -bench BenchmarkFieldPath -benchmem -count=12 .`

`-count=12` runs the whole set twelve times, so the two configurations alternate — that is the interleaving the global constraints require. Record allocs/op for both. Expected, from the spike this plan is built on: about 19 for bare and about 105 for the interceptor. If your numbers differ materially, report them — they are the baseline every later task is measured against, and the spike's figures are not privileged over yours.

- [ ] **Step 3: Commit**

```bash
git add bench_fieldpath_test.go
git commit -m "test: benchmark the field interceptor path"
```

---

### Task 2: Pass the FieldContext instead of attaching it

**Files:**
- Modify: `plan.go:96-99` (`fieldExec`), `plan.go:357`
- Modify: `object.go:69`, `object.go:92`, `object.go:209` (and the matching `resolve` declarations beside them)
- Modify: `exec_object.go:105-136` (`fieldContext`, `callLeaf`, `callResolve`)
- Modify: `interceptor.go:157-184` (`interceptedExec`)
- Modify: `context.go` (`FieldContext.path` becomes lazy)
- Modify: `subscription.go:270-274`
- Test: `interceptor_test.go`, `context_test.go`

**Interfaces:**
- Consumes: Task 1's `BenchmarkFieldPathInterceptor` as the instrument.
- Produces, used by Task 3:
  - `fieldExec.writeLeaf func(ctx context.Context, w *jsonw.Writer, parent, args any, fc *FieldContext) error`
  - `fieldExec.resolve func(ctx context.Context, parent, args any, fc *FieldContext) (any, error)`
  - `(*execState).fieldContext(ctx, f, parent, args, path) (context.Context, *FieldContext)`

- [ ] **Step 1: Write the failing tests**

Add to `interceptor_test.go`:

```go
// TestFieldInterceptorSeesPureFieldsWithoutContextAttachment pins the split
// this change introduces: an interceptor still receives every field's
// FieldContext as its parameter, but the context no longer carries one for a
// pure field, because nothing but the interceptor could read it there --
// Field and FieldArgs accessors take no context at all.
func TestFieldInterceptorSeesPureFieldsWithoutContextAttachment(t *testing.T) {
	var seen []string
	var attached []bool
	e := newFixtureExecutor(t, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			if fc == nil {
				t.Error("interceptor got a nil FieldContext")
				return next(ctx)
			}
			seen = append(seen, fc.Object.Name+"."+fc.Field.Name)
			attached = append(attached, FieldFrom(ctx) != nil)
			return next(ctx)
		})))

	run(t, e, `{ users { id } }`, "")

	if len(seen) == 0 {
		t.Fatal("interceptor never ran")
	}
	for i, name := range seen {
		if name == "Query.users" {
			continue // a resolver field: the context must still carry it
		}
		if attached[i] {
			t.Errorf("%s: context still carries a FieldContext for a pure field", name)
		}
	}
}
```

Add to `context_test.go`:

```go
// TestPathFromInsideResolver pins the public guarantee this change must not
// touch: a resolver field can still read its own path out of the context.
func TestPathFromInsideResolver(t *testing.T) {
	var got Path
	s, err := NewSchema(SDL(`type Query { deep: Inner } type Inner { v: String }`),
		Object[Root]("Query", Resolve("deep", func(ctx context.Context, _ Root) (*inner, error) {
			got = PathFrom(ctx)
			return &inner{}, nil
		})),
		Object[inner]("Inner", Field("v", func(*inner) string { return "x" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	run(t, NewExecutor(s), `{ deep { v } }`, "")
	if len(got) != 1 || got[0] != "deep" {
		t.Fatalf("PathFrom = %v, want [deep]", got)
	}
}

type inner struct{}
```

Check `fixture_test.go` for whether `inner` or a similar name is taken before adding it, and for `run`'s exact signature — it is the shared helper and this plan assumes `run(t, e, query, opName)`.

- [ ] **Step 2: Run them and verify the first fails**

Run: `go test -race -count=1 -run 'TestFieldInterceptorSeesPureFields|TestPathFromInsideResolver' .`
Expected: `TestFieldInterceptorSeesPureFields...` FAILS (today the context carries a FieldContext for pure fields when interceptors are registered). `TestPathFromInsideResolver` PASSES — it pins behaviour that must survive, so it is green before and after.

- [ ] **Step 3: Widen the exec signatures**

In `plan.go`, `fieldExec`:

```go
type fieldExec struct {
	writeLeaf func(ctx context.Context, w *jsonw.Writer, parent, args any, fc *FieldContext) error
	resolve   func(ctx context.Context, parent, args any, fc *FieldContext) (any, error)
}
```

Make the same two signature changes to `fieldDef.writeLeaf` and `fieldDef.resolve` in `object.go:69` and its neighbour, and to the two closures that build them (`object.go:92`, `object.go:209`). Those closures ignore the new parameter — name it `_`. There is no adapter and no extra indirection: `plan.go:357`'s `fieldExec{writeLeaf: fd.writeLeaf, resolve: fd.resolve}` keeps working as a direct assignment, which is the whole reason for widening the shared type rather than wrapping.

Update `subscription.go:270-274`'s substituted functions to the new signatures; they ignore the parameter too.

- [ ] **Step 4: Return the FieldContext rather than only attaching it**

In `exec_object.go`:

```go
// fieldContext builds the FieldContext for a field and attaches it to the
// context only where something could read it back: a resolver may call
// FieldFrom or PathFrom, a pure accessor takes no context at all. An
// interceptor receives it as an argument either way.
func (st *execState) fieldContext(ctx context.Context, f *planField, parent, args any, path *pathNode) (context.Context, *FieldContext) {
	fd := f.def
	if fd.pure && len(st.e.fieldInterceptors) == 0 {
		return ctx, nil
	}
	fc := &FieldContext{Field: fd.def, Object: fd.object.def, Args: args, Parent: parent, field: f, pathParent: path, alias: f.alias}
	if fd.pure {
		return ctx, fc
	}
	return withField(ctx, fc), fc
}
```

and thread it through both callers:

```go
func (st *execState) callLeaf(ctx context.Context, w *jsonw.Writer, f *planField, parent, args any, path *pathNode) (err error) {
	ctx, fc := st.fieldContext(ctx, f, parent, args, path)
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				err = st.recovered(ctx, r, path, f)
			}
		}()
	}
	return f.exec.writeLeaf(ctx, w, parent, args, fc)
}

func (st *execState) callResolve(ctx context.Context, f *planField, parent, args any, path *pathNode) (v any, err error) {
	ctx, fc := st.fieldContext(ctx, f, parent, args, path)
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				v, err = nil, st.recovered(ctx, r, path, f)
			}
		}()
	}
	return f.exec.resolve(ctx, parent, args, fc)
}
```

- [ ] **Step 5: Make the path node lazy**

In `context.go`, replace `FieldContext`'s `path *pathNode` field with the two values it is built from, and materialize on demand:

```go
type FieldContext struct {
	Field  *ast.FieldDefinition
	Object *ast.Definition
	Args   any
	Parent any

	field      *planField
	pathParent *pathNode
	alias      string
}

// Path returns the response path of the field. The node is built here rather
// than when the FieldContext is, because most fields are never asked for it.
func (fc *FieldContext) Path() Path {
	n := pathNode{parent: fc.pathParent, key: fc.alias}
	return n.materialize()
}
```

`materialize` reads the node and returns a `Path`; it does not retain the node, so this one stays on the stack.

- [ ] **Step 6: Take the FieldContext from the argument in interceptedExec**

In `interceptor.go`, `interceptedExec`'s `chain` currently opens with `fc := FieldFrom(ctx)`. Delete that line and take `fc` from the new parameter instead, so both returned closures match the widened `fieldExec` types:

```go
chain := func(ctx context.Context, parent, args any, fc *FieldContext) (any, error) {
	handler := FieldHandler(func(ctx context.Context) (any, error) { return inner(ctx, parent, args) })
	for i := len(e.fieldInterceptors) - 1; i >= 0; i-- {
		next, fi := handler, e.fieldInterceptors[i]
		handler = func(ctx context.Context) (any, error) { return fi.InterceptField(ctx, fc, next) }
	}
	return handler(ctx)
}
```

and update the two `fieldExec` literals it returns to take and forward `fc`.

- [ ] **Step 7: Run the tests**

Run: `go vet ./... && go test -race -count=1 .`
Expected: PASS, including both tests from step 1.

One failure is expected and is not a bug: any existing test whose interceptor reads `FieldFrom(ctx)` rather than its `fc` parameter now sees nil on a pure field. Spec §4 records this as a deliberate pre-1.0 break. Change such a test to use the parameter and say so in the commit message. If a test fails for any other reason, stop and report.

- [ ] **Step 8: Measure**

Run: `go test -run '^$' -bench BenchmarkFieldPath -benchmem -count=12 .`
Compare allocs/op for `BenchmarkFieldPathInterceptor` against Task 1's number. Expect a fall — the `context.WithValue` and the eager path node are both gone for pure fields — but **do not predict a specific figure in the commit message; report what you measured.**

- [ ] **Step 9: Commit**

```bash
git add plan.go object.go exec_object.go interceptor.go context.go subscription.go interceptor_test.go context_test.go
git commit -m "perf: pass the FieldContext to interceptors instead of attaching it

An interceptor already receives the FieldContext as a parameter; it went
through the context only because fieldExec's signature could not carry it.
Widening that signature costs nothing at runtime -- the two closures that build
a typed leaf writer ignore the new parameter -- and lets the attachment happen
only where a resolver could read it back. The path node is built when Path() is
called rather than for every field."
```

---

### Task 3: The FieldObserver

**Files:**
- Create: `observer.go`
- Create: `observer_test.go`
- Modify: `exec.go` (executor field + option), `exec_object.go` (`callLeaf`, `callResolve`)
- Modify: `bench_fieldpath_test.go`

**Interfaces:**
- Consumes: Task 2's `fieldContext` returning `(context.Context, *FieldContext)`, and its widened `fieldExec` types.
- Produces, used by Task 4:
  - `type FieldInfo struct { Object, Field, Alias string; pathParent *pathNode }`
  - `func (f FieldInfo) Path() Path`
  - `type FieldObserver interface { BeginField(context.Context, FieldInfo) context.Context; EndField(context.Context, FieldInfo, error) }`
  - `func WithFieldObserver(o ...FieldObserver) ExecutorOption`

- [ ] **Step 1: Write the failing tests**

Create `observer_test.go`:

```go
package graphql

import (
	"context"
	"testing"
)

type recordingObserver struct {
	begun []string
	ended []string
	errs  []error
	depth []any
}

type obsKey struct{}

func (o *recordingObserver) BeginField(ctx context.Context, f FieldInfo) context.Context {
	o.begun = append(o.begun, f.Object+"."+f.Field)
	return context.WithValue(ctx, obsKey{}, f.Object+"."+f.Field)
}

func (o *recordingObserver) EndField(ctx context.Context, f FieldInfo, err error) {
	o.ended = append(o.ended, f.Object+"."+f.Field)
	o.errs = append(o.errs, err)
	o.depth = append(o.depth, ctx.Value(obsKey{}))
}

// TestFieldObserverSeesPureFields is the point of the whole change: a pure
// field is observed without an interceptor, and therefore without leaving its
// typed write path.
func TestFieldObserverSeesPureFields(t *testing.T) {
	o := &recordingObserver{}
	e := newFixtureExecutor(t, WithFieldObserver(o))
	run(t, e, `{ users { id } }`, "")

	if len(o.begun) == 0 {
		t.Fatal("observer never ran")
	}
	var sawPure bool
	for _, name := range o.begun {
		if name == "User.id" {
			sawPure = true
		}
	}
	if !sawPure {
		t.Errorf("observer never saw the pure field User.id; saw %v", o.begun)
	}
	if len(o.ended) != len(o.begun) {
		t.Errorf("%d BeginField against %d EndField", len(o.begun), len(o.ended))
	}
}

// TestFieldObserverEndSeesBeginContext pins the contract that lets an observer
// keep state without storing it anywhere: EndField gets the context BeginField
// returned, which is where tracer.Start already puts its span.
func TestFieldObserverEndSeesBeginContext(t *testing.T) {
	o := &recordingObserver{}
	e := newFixtureExecutor(t, WithFieldObserver(o))
	run(t, e, `{ users { id } }`, "")

	for i, v := range o.depth {
		if v != o.ended[i] {
			t.Errorf("EndField %d saw context value %v, want %q", i, v, o.ended[i])
		}
	}
}

// TestFieldObserverKeepsTypedWritePath is the structural assertion. Timing
// cannot tell the two paths apart reliably on this machine; an allocation
// bound can, because the type-erased path allocates per leaf and the typed one
// does not.
func TestFieldObserverKeepsTypedWritePath(t *testing.T) {
	bare := testing.Benchmark(func(b *testing.B) { benchFieldPath(b) })
	obs := testing.Benchmark(func(b *testing.B) {
		benchFieldPath(b, WithFieldObserver(&countingObserver{}))
	})
	// A countingObserver allocates nothing of its own, so any gap is the
	// engine's. The type-erased path costs tens of allocations on this query;
	// a handful of slack absorbs observer bookkeeping without absorbing that.
	if got, want := obs.AllocsPerOp(), bare.AllocsPerOp()+8; got > want {
		t.Fatalf("observer path allocates %d/op against %d/op bare; want at most %d",
			got, bare.AllocsPerOp(), want)
	}
}

type countingObserver struct{ n int }

func (c *countingObserver) BeginField(ctx context.Context, f FieldInfo) context.Context {
	c.n++
	return ctx
}
func (c *countingObserver) EndField(context.Context, FieldInfo, error) {}
```

- [ ] **Step 2: Run them and verify they fail**

Run: `go test -race -count=1 -run TestFieldObserver .`
Expected: FAIL to compile — `WithFieldObserver`, `FieldInfo` and `FieldObserver` do not exist.

- [ ] **Step 3: Create observer.go**

```go
package graphql

import "context"

// FieldInfo identifies a field to an observer. It is a value, and its path is
// materialized only if asked for, so observing a field costs nothing until the
// observer itself spends something.
//
// It deliberately carries neither the arguments nor the parent value: those
// are what a FieldContext is for, and wanting them means wanting a
// FieldInterceptor.
type FieldInfo struct {
	Object string
	Field  string
	Alias  string

	pathParent *pathNode
}

// Path returns the response path of the field.
func (f FieldInfo) Path() Path {
	n := pathNode{parent: f.pathParent, key: f.Alias}
	return n.materialize()
}

// FieldObserver watches every field, pure ones included, without seeing its
// value. Not seeing the value is what lets the engine leave each field on its
// typed write path: a FieldInterceptor, which may replace a result, cannot.
type FieldObserver interface {
	// BeginField runs before the field. The context it returns is the one the
	// field and its children run under, so an observer that starts a span
	// returns the context carrying it.
	BeginField(ctx context.Context, f FieldInfo) context.Context

	// EndField runs after the field, with the context BeginField returned.
	EndField(ctx context.Context, f FieldInfo, err error)
}

// WithFieldObserver registers field observers. The first is outermost, as with
// the interceptor options.
func WithFieldObserver(o ...FieldObserver) ExecutorOption {
	return func(e *Executor) { e.fieldObservers = append(e.fieldObservers, o...) }
}
```

- [ ] **Step 4: Add the executor field**

In `exec.go`, add `fieldObservers []FieldObserver` beside `fieldInterceptors`. This is on `Executor`, which is per-executor rather than per-request, so the size-class rule for `execState` and `OperationContext` does not apply — but say so in the commit message so the next reader does not have to re-derive it.

- [ ] **Step 5: Call observers from the two choke points**

In `exec_object.go`, wrap the body of `callLeaf` and `callResolve`. `fieldInfo` reads from the `*planField` the executor already holds, so it allocates nothing:

```go
func (st *execState) fieldInfo(f *planField, path *pathNode) FieldInfo {
	fd := f.def
	return FieldInfo{Object: fd.object.name, Field: fd.name, Alias: f.alias, pathParent: path}
}
```

In `callLeaf`, after the `fieldContext` call and before the executor call:

```go
	if len(st.e.fieldObservers) > 0 {
		fi := st.fieldInfo(f, path)
		for _, o := range st.e.fieldObservers {
			ctx = o.BeginField(ctx, fi)
		}
		defer func() {
			for i := len(st.e.fieldObservers) - 1; i >= 0; i-- {
				st.e.fieldObservers[i].EndField(ctx, fi, err)
			}
		}()
	}
```

`err` is the named return, so the deferred call sees the field's outcome. Apply the same block to `callResolve`, whose named returns are `(v any, err error)`.

- [ ] **Step 6: Run the tests**

Run: `go vet ./... && go test -race -count=1 .`
Expected: PASS, including all three observer tests.

If `TestFieldObserverKeepsTypedWritePath` fails, the observer is routing fields through the type-erased path — do not raise the bound to make it pass. The bound is the test.

- [ ] **Step 7: Add the observer benchmark**

Append to `bench_fieldpath_test.go`:

```go
// BenchmarkFieldPathObserver is the counterpart to
// BenchmarkFieldPathInterceptor: the same no-op observation, against an
// observer that cannot see the value and so does not force type erasure.
func BenchmarkFieldPathObserver(b *testing.B) {
	benchFieldPath(b, WithFieldObserver(noopObserver{}))
}

type noopObserver struct{}

func (noopObserver) BeginField(ctx context.Context, _ FieldInfo) context.Context { return ctx }
func (noopObserver) EndField(context.Context, FieldInfo, error)                  {}
```

Run: `go test -run '^$' -bench BenchmarkFieldPath -benchmem -count=12 .`
Record all three. Report the numbers; do not assert a predicted figure.

- [ ] **Step 8: Verify the test discriminates**

Temporarily change `callLeaf` so that registering an observer also forces the interceptor path — the simplest way is to make `fieldContext`'s pure short-circuit also check `len(st.e.fieldObservers)` and then route the leaf through `interceptedExec`'s shape. If that is awkward, instead make `noopObserver.BeginField` allocate a large slice per call and confirm the bound test fails; then say in your report which break you used and what it proves.

Expected: `TestFieldObserverKeepsTypedWritePath` FAILS. Restore by reverse edit, confirm it passes.

- [ ] **Step 9: Commit**

```bash
git add observer.go observer_test.go exec.go exec_object.go bench_fieldpath_test.go
git commit -m "feat: add FieldObserver, which observes a field without erasing its type

A FieldInterceptor may replace a field's result, so every field it watches
leaves its typed writer for anyResolve and writeAny. An observer cannot, so it
does not. BeginField returns the context the field runs under and EndField
receives it back, which is where tracer.Start already keeps its span -- so the
machinery costs no closure and no boxing."
```

---

### Task 4: Switch ext/otel's field spans to the observer

**Files:**
- Modify: `ext/otel/otel.go:120-121`, `ext/otel/otel.go:211-226`
- Modify: `ext/otel/otel_test.go`

**Interfaces:**
- Consumes: `graphql.FieldObserver`, `graphql.FieldInfo`, `graphql.WithFieldObserver` from Task 3.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Write the failing test**

`ext/otel/otel_test.go` already has field span coverage. Add one that pins the property this task exists for — that field spans no longer drag every field through the interceptor path:

```go
// TestFieldSpansUseObserver pins that field spans are wired through the
// observer rather than the interceptor. The distinction is not cosmetic: an
// interceptor routes every field, pure ones included, through the type-erased
// write path, which is what made field spans cost four times the request.
func TestFieldSpansUseObserver(t *testing.T) {
	var sawObserver bool
	for _, opt := range otel.New(otel.WithFieldSpans(true)) {
		e := &graphql.Executor{}
		_ = opt
		_ = e
		sawObserver = true // replaced below
	}
	_ = sawObserver
}
```

That sketch does not work — `Executor`'s fields are unexported and `ExecutorOption` cannot be inspected from another package. Write it instead as a behavioural test in `ext/otel`: build an executor with `otel.New(otel.WithFieldSpans(true))`, execute a query whose fields are all pure, and assert from the recorded spans that a field span exists. Then assert the allocation property in the root package where it can be reached, or leave the allocation claim to `TestFieldObserverKeepsTypedWritePath` from Task 3 and say so in your report. **Decide which, implement one, and state the choice** — do not write a test that asserts nothing.

- [ ] **Step 2: Run it and verify it fails**

Run: `cd ext/otel && go test -race -count=1 -run TestFieldSpans ./...`
Expected: FAIL — field spans still come from the interceptor.

- [ ] **Step 3: Switch the wiring**

In `ext/otel/otel.go`, replace

```go
	if c.fieldSpans {
		out = append(out, graphql.WithFieldInterceptor(graphql.FieldInterceptorFunc(c.interceptField)))
	}
```

with

```go
	if c.fieldSpans {
		out = append(out, graphql.WithFieldObserver(fieldSpanObserver{c}))
	}
```

- [ ] **Step 4: Replace interceptField with the observer**

Delete `interceptField` and add:

```go
// fieldSpanObserver emits a span per field. It keeps no state: Start returns a
// context carrying the span and EndField is handed that same context back, so
// there is nothing to store between the two.
type fieldSpanObserver struct{ c *config }

func (o fieldSpanObserver) BeginField(ctx context.Context, f graphql.FieldInfo) context.Context {
	ctx, span := o.c.tracer.Start(ctx, f.Object+"."+f.Field)
	span.SetAttributes(
		AttrFieldObject.String(f.Object),
		AttrFieldPath.String(f.Path().String()),
	)
	return ctx
}

func (o fieldSpanObserver) EndField(ctx context.Context, _ graphql.FieldInfo, err error) {
	span := trace.SpanFromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
```

The span name, both attributes and the error recording are copied from `interceptField` so the emitted telemetry is unchanged.

- [ ] **Step 5: Run the otel suite**

Run: `cd ext/otel && go vet ./... && go test -race -count=1 ./...`
Expected: PASS. Existing field span tests must pass **unchanged** — same names, same attributes, same nesting. If one needs editing, that is a behaviour change and you should report it rather than edit the test.

- [ ] **Step 6: Run the full gate**

Run: `sh scripts/gate.sh`
Expected: all modules pass; `compare` skipped for missing fixtures, which is expected.

- [ ] **Step 7: Measure the end state**

Run: `go test -run '^$' -bench BenchmarkFieldPath -benchmem -count=12 .` from the repository root and report all three configurations against Task 1's baseline. This is the number the spec's §7 prediction is checked against — report it whether or not it matches.

- [ ] **Step 8: Commit**

```bash
git add ext/otel/otel.go ext/otel/otel_test.go
git commit -m "perf: emit field spans from an observer instead of an interceptor

interceptField never read the value it was handed; it used the object name, the
field name, the path and the error. Paying for a type-erased round trip to
deliver a value the only field interceptor in the repository discards is what
made WithFieldSpans cost what it did."
```

- [ ] **Step 9: Update CLAUDE.md**

The architecture section says field spans "are opt-in and cost more than they look: a field interceptor routes every field through the type-erased path, pure ones included." That is now half the story. Rewrite it from the measurements you took: what an interceptor costs, what an observer costs, and why the difference is that an observer cannot change a result. Use your own numbers, not this plan's.

```bash
git add CLAUDE.md
git commit -m "docs: record what field observation costs against field interception"
```

---

## Self-Review

**Spec coverage**

| Spec section | Task |
|---|---|
| §1 measurement baseline | Task 1 |
| §3 observer interface | Task 3 steps 3-5 |
| §3.1 no closure | Task 3 step 3 (two methods, context carries state) |
| §3.2 `FieldInfo` is a value, lazy path | Task 3 step 3 |
| §4.1 pass rather than attach | Task 2 steps 3-4, 6 |
| §4.2 lazy path node | Task 2 step 5 |
| §4 observable break | Task 2 step 1 (test), step 7 (expected failure) |
| §5 `WithFieldObserver` | Task 3 step 3 |
| §5 otel switch | Task 4 |
| §6 benchmark first | Task 1, by position |
| §6 observer sees every field | Task 3 step 1 |
| §6 nesting contract | Task 3 step 1 (`TestFieldObserverEndSeesBeginContext`) |
| §6 typed path, structurally | Task 3 step 1 (`TestFieldObserverKeepsTypedWritePath`) |
| §6 interceptor still works | Task 2 step 7 |
| §6 `FieldFrom` contract | Task 2 step 1 (`TestPathFromInsideResolver`) |
| §6 otel parity | Task 4 step 5 |
| §6 deliberate break | Task 3 step 8 |
| §7 prediction checked | Task 4 step 7 |

**Type consistency** — `fieldExec`'s widened signatures are defined in Task 2 and consumed unchanged in Task 3. `FieldInfo`/`FieldObserver`/`WithFieldObserver` are defined in Task 3 and consumed in Task 4. `fieldContext` returns `(context.Context, *FieldContext)` from Task 2 onward and Task 3's step 5 depends on that shape.

**Known soft spots, flagged rather than hidden**

- Task 2 step 1 assumes `run(t, e, query, opName)` and that the name `inner` is free. Both are checked in the step rather than asserted.
- Task 3 step 8's deliberate break is described two ways because the clean version may be awkward; the step requires the implementer to say which they used and what it proves.
- Task 4 step 1 does not contain a working test. It contains a sketch that is explicitly marked as not working, two viable alternatives, and an instruction to pick one and say so. This is the one place the plan asks for judgment instead of transcription, because the assertion has to live where the thing it asserts is reachable.
