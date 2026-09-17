# Response Size Bound Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `WithMaxResponseBytes`, which replaces an oversized response's data with `null` and a single `RESPONSE_TOO_LARGE` error, and stops execution early once the limit is passed.

**Architecture:** The byte budget lives on the pooled `jsonw.Writer` (root embeds it, concurrent sub-writers share the root's), so `execState` does not grow. The executor checks it before every field and decides exactly on the finished root writer.

**Tech Stack:** Go 1.27, standard library only in the root package.

**Spec:** `docs/superpowers/specs/2026-09-17-max-response-bytes-design.md`

## Global Constraints

- Root package imports only `gqlparser/v2` and the standard library; `internal/jsonw` imports no engine types.
- `execState` stays 64 bytes and `OperationContext` 160 bytes (`TestStructSizes` logs both).
- No reflection on the request path.
- With no limit configured, the write path adds at most one nil compare per field and one pointer store per sub-writer; `BenchmarkFieldPathBare` stays at 18 allocs/op.
- Error code string is exactly `RESPONSE_TOO_LARGE`; message is exactly `response exceeds the maximum size of %d bytes`.
- Every new test is broken on purpose once and must fail; undo breaks with a reverse edit, never `git checkout -- <file>`.
- Gate: `go vet ./... && go test -race -count=1 ./...` from the worktree root.
- Comments explain why, English only. Commit messages: `feat:`/`test:`/`docs:` prefix, imperative, lower case, ending with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

### Task 1: Byte budget on the JSON writer

**Files:**
- Modify: `internal/jsonw/writer.go` (the `Writer` struct, `Reset`, new methods)
- Test: `internal/jsonw/writer_test.go`

**Interfaces:**
- Produces: `func (w *Writer) Limit(n int64)`, `func (w *Writer) ShareLimit(from *Writer)`, `func (w *Writer) OverLimit() bool`, `func (w *Writer) LimitExceeded() bool`. `Reset` gives back a sub-writer's reported bytes and clears all budget state.

- [ ] **Step 1: Write the failing tests** — append to `internal/jsonw/writer_test.go` (add `"strings"` to its imports):

```go
func TestLimitUnset(t *testing.T) {
	w := New()
	w.String(strings.Repeat("x", 100))
	if w.OverLimit() || w.LimitExceeded() {
		t.Fatal("a writer with no limit must never be over it")
	}
}

// TestLimitReportsAndLatches pins that the budget tracks the buffer exactly,
// including shrinking after a rewind, and that once exceeded it stays exceeded:
// execution must keep stopping even after null bubbling rewinds the bytes away.
func TestLimitReportsAndLatches(t *testing.T) {
	w := New()
	w.Limit(10)
	w.BeginArray()
	w.String("ab")
	if w.OverLimit() {
		t.Fatalf("over at %d bytes against a limit of 10", w.Len())
	}
	if got := w.budget.used.Load(); got != int64(w.Len()) {
		t.Fatalf("used = %d, want %d", got, w.Len())
	}
	m := w.Mark()
	w.String("abcdefgh")
	if !w.OverLimit() {
		t.Fatalf("not over at %d bytes against a limit of 10", w.Len())
	}
	w.Rewind(m)
	if !w.OverLimit() {
		t.Fatal("exceeded must latch after a rewind")
	}
	if got := w.budget.used.Load(); got != int64(w.Len()) {
		t.Fatalf("after rewind used = %d, want %d", got, w.Len())
	}
	if !w.LimitExceeded() {
		t.Fatal("LimitExceeded is false after OverLimit returned true")
	}
}

// TestLimitSharedAcrossWriters is why the budget is a pointer: neither writer
// alone passes the limit, together they do.
func TestLimitSharedAcrossWriters(t *testing.T) {
	root := New()
	root.Limit(20)
	a, b := New(), New()
	a.ShareLimit(root)
	b.ShareLimit(root)
	a.String("123456789") // 11 bytes
	if a.OverLimit() {
		t.Fatal("11 bytes against a limit of 20 is not over")
	}
	b.String("123456789")
	if !b.OverLimit() {
		t.Fatal("two writers sharing a limit of 20 hold 22 bytes and b was not over")
	}
	if !root.LimitExceeded() {
		t.Fatal("the root does not see its sub-writers' excess")
	}
}

// TestLimitResetGivesBack pins that a sub-writer returned to the pool takes its
// bytes out of the shared count, and that a reset writer carries no budget into
// the next request that gets it from the pool.
func TestLimitResetGivesBack(t *testing.T) {
	root := New()
	root.Limit(1000)
	sub := New()
	sub.ShareLimit(root)
	sub.String("123456789")
	sub.OverLimit()
	root.BeginArray()
	root.OverLimit()

	sub.Reset()
	if got, want := root.budget.used.Load(), int64(root.Len()); got != want {
		t.Fatalf("after the sub-writer reset, used = %d, want the root's own %d", got, want)
	}
	if sub.budget != nil || sub.reported != 0 {
		t.Fatal("a reset sub-writer still carries budget state")
	}

	root.String(strings.Repeat("x", 2000))
	root.OverLimit()
	root.Reset()
	if root.budget != nil || root.reported != 0 || root.own.used.Load() != 0 || root.own.limit != 0 || root.own.exceeded.Load() {
		t.Fatal("a reset root writer still carries budget state")
	}
	if root.OverLimit() {
		t.Fatal("a reset writer is over a limit it no longer has")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/jsonw/ -run TestLimit`
Expected: build failure, `w.Limit undefined` (and the other new names).

- [ ] **Step 3: Implement** in `internal/jsonw/writer.go`. Add `"sync/atomic"` to imports. Replace the `Writer` struct with:

```go
// Writer appends JSON to an internal buffer.
//
// Every value-writing method emits the separator required by the enclosing
// container, so callers never write commas themselves. Key writes the
// separator and the key; the value written immediately after it must not
// emit another separator, which the writer tracks with a pending-key flag.
type Writer struct {
	buf        []byte
	stack      []bool // per open container: whether a value has been written
	pendingKey bool

	// budget is nil when unlimited, &own on a root writer, and the root's on a
	// sub-writer, so concurrently written buffers count against one limit. It
	// lives here rather than on the executor's per-request state because the
	// writer is pooled: growing it costs no allocation per request.
	budget   *budget
	reported int // bytes of buf already added to budget.used
	own      budget
}

// budget is a byte limit shared by a root writer and its sub-writers.
type budget struct {
	used     atomic.Int64
	limit    int64
	exceeded atomic.Bool
}
```

Replace `Reset` with:

```go
// Reset clears the buffer and all container state. A writer sharing another's
// limit gives back the bytes it reported, so a buffer returned to the pool
// stops counting against the response it was part of.
func (w *Writer) Reset() {
	if w.budget != nil {
		if w.budget != &w.own {
			w.budget.used.Add(-int64(w.reported))
		}
		w.budget = nil
		w.reported = 0
		w.own.used.Store(0)
		w.own.limit = 0
		w.own.exceeded.Store(false)
	}
	w.buf = w.buf[:0]
	w.stack = w.stack[:0]
	w.pendingKey = false
}
```

Add after `Len`:

```go
// Limit bounds the bytes held by w and every writer sharing its limit. It is
// called on a reset writer before anything is written.
func (w *Writer) Limit(n int64) {
	w.own.limit = n
	w.budget = &w.own
}

// ShareLimit makes w count against from's limit, if from has one.
func (w *Writer) ShareLimit(from *Writer) {
	w.budget = from.budget
}

// OverLimit reports this writer's growth since its last call and whether the
// shared limit has been passed. Once passed it stays passed, even if rewinds
// later shrink the count, so execution keeps stopping. With no limit it is a
// nil compare, kept small enough to inline.
func (w *Writer) OverLimit() bool {
	if w.budget == nil {
		return false
	}
	return w.overLimit()
}

// overLimit reports on every call rather than in fixed-size blocks: a block
// would only be reported once a writer grew a whole one, and a concurrent list
// element's buffer is usually far smaller, so thousands of them would never
// report at all.
func (w *Writer) overLimit() bool {
	b := w.budget
	if d := len(w.buf) - w.reported; d != 0 {
		b.used.Add(int64(d))
		w.reported = len(w.buf)
	}
	if b.exceeded.Load() {
		return true
	}
	if b.used.Load() > b.limit {
		b.exceeded.Store(true)
		return true
	}
	return false
}

// LimitExceeded reports whether any OverLimit call on this writer's limit
// found it passed.
func (w *Writer) LimitExceeded() bool {
	return w.budget != nil && w.budget.exceeded.Load()
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/jsonw/ && go build -gcflags=-m ./internal/jsonw/ 2>&1 | grep -E "can inline \(\*Writer\)\.OverLimit"`
Expected: `ok`, and the grep prints a `can inline (*Writer).OverLimit` line.

- [ ] **Step 5: Deliberate breaks** — each must make a test fail, then reverse the edit:
  1. In `Reset`, delete the `w.budget.used.Add(-int64(w.reported))` line → `TestLimitResetGivesBack` fails.
  2. In `overLimit`, delete the `if b.exceeded.Load() { return true }` block → `TestLimitReportsAndLatches` fails ("must latch").
  3. In `ShareLimit`, make the body empty → `TestLimitSharedAcrossWriters` fails.

- [ ] **Step 6: Commit**

```bash
git add internal/jsonw/writer.go internal/jsonw/writer_test.go
git commit -m "feat: give the json writer a byte budget shared with its sub-writers"
```

---

### Task 2: WithMaxResponseBytes in the executor

**Files:**
- Modify: `errors.go` (new code), `limits.go` (option), `exec.go` (Executor field, `runOperation`, new helpers), `exec_object.go` (`writeFieldValue` checkpoint, `ShareLimit` in both concurrent writers), `subscription.go` (`runSubscriptionEvent`)
- Create: `response_limit_test.go`

**Interfaces:**
- Consumes: Task 1's `Limit`, `ShareLimit`, `OverLimit`, `LimitExceeded`.
- Produces: `func WithMaxResponseBytes(n int64) ExecutorOption`, `const CodeResponseTooLarge = "RESPONSE_TOO_LARGE"`, `Executor.maxResponseBytes int64`, `func (e *Executor) newResponseWriter() *jsonw.Writer`, `func (st *execState) finishData(ctx context.Context, w *jsonw.Writer, ok bool)`.

- [ ] **Step 1: Write the failing tests** — create `response_limit_test.go`:

```go
package graphql

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const limitSDL = `
type Item { body: String! }
type Section { items: [Item!]! }
type Query {
  short: String!
  failNullable: String
  a: Section!
  b: Section!
}
`

type limitItem struct{ n int }
type limitSection struct{ n int }

// limitFixture counts how many item bodies were written, which is how a test
// tells an execution that stopped early from one that wrote everything and was
// only rejected at the end.
type limitFixture struct{ bodies atomic.Int64 }

func newLimitExecutor(t *testing.T, opts ...ExecutorOption) (*limitFixture, *Executor) {
	t.Helper()
	f := &limitFixture{}
	items := make([]*limitItem, 1000)
	for i := range items {
		items[i] = &limitItem{n: i}
	}
	body := strings.Repeat("x", 100)
	s, err := NewSchema(SDL(limitSDL),
		Object[limitItem]("Item",
			Field("body", func(*limitItem) string {
				f.bodies.Add(1)
				return body
			}),
		),
		Object[limitSection]("Section",
			Field("items", func(*limitSection) []*limitItem { return items }),
		),
		Query(
			Field("short", func(Root) string { return "abc" }),
			Resolve("failNullable", func(context.Context, Root) (*string, error) { return nil, errors.New("boom") }),
			Resolve("a", func(context.Context, Root) (*limitSection, error) { return &limitSection{}, nil }),
			Resolve("b", func(context.Context, Root) (*limitSection, error) { return &limitSection{}, nil }),
		),
	)
	if err != nil {
		t.Fatalf("limit schema: %v", err)
	}
	return f, NewExecutor(s, opts...)
}

func expectTooLarge(t *testing.T, resp *Response, limit int64) {
	t.Helper()
	if got := string(resp.Data); got != "null" {
		t.Fatalf("data = %.200s, want null", got)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("want exactly one error, got %s", errorsJSON(resp.Errors))
	}
	err := resp.Errors[0]
	if err.Extensions["code"] != CodeResponseTooLarge {
		t.Errorf("code = %v, want %s", err.Extensions["code"], CodeResponseTooLarge)
	}
	if want := "response exceeds the maximum size of " + itoa(limit) + " bytes"; err.Message != want {
		t.Errorf("message = %q, want %q", err.Message, want)
	}
	if err.Path != nil || len(err.Locations) != 0 {
		t.Errorf("a response-level error has path %v and locations %v", err.Path, err.Locations)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestResponseLimitExactBoundary pins that the final decision is exact, not
// the approximate in-flight count: {"short":"abc"} is 15 bytes.
func TestResponseLimitExactBoundary(t *testing.T) {
	_, fits := newLimitExecutor(t, WithMaxResponseBytes(15))
	expectData(t, run(t, fits, `{ short }`, ""), `{"short":"abc"}`)

	_, over := newLimitExecutor(t, WithMaxResponseBytes(14))
	expectTooLarge(t, run(t, over, `{ short }`, ""), 14)
}

// TestResponseLimitReplacesErrors pins the failure shape: the field error from
// failNullable points into data that no longer exists, so it must not survive.
func TestResponseLimitReplacesErrors(t *testing.T) {
	_, e := newLimitExecutor(t, WithMaxConcurrency(0), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ short failNullable a { items { body } } }`, ""), 8<<10)
}

// TestResponseLimitStopsSequentialExecution is the difference between a limit
// and a post-hoc check: the 1,000 items would be written in full by a check
// that only looked at the finished response.
func TestResponseLimitStopsSequentialExecution(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(0), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ a { items { body } } }`, ""), 8<<10)
	if n := f.bodies.Load(); n >= 500 {
		t.Fatalf("wrote %d of 1000 bodies under an 8 KiB limit; execution did not stop early", n)
	}
}

// TestResponseLimitStopsSubWriters covers the concurrent path, where a and b
// each write into their own sub-writer and nothing reaches the root writer
// until both finish. Without the shared budget both write all 1,000 items.
func TestResponseLimitStopsSubWriters(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(4), WithMaxResponseBytes(8<<10))
	expectTooLarge(t, run(t, e, `{ a { items { body } } b { items { body } } }`, ""), 8<<10)
	if n := f.bodies.Load(); n >= 1000 {
		t.Fatalf("wrote %d of 2000 bodies under an 8 KiB limit; sub-writers did not share the budget", n)
	}
}

// TestResponseLimitUnsetWritesEverything guards the test above against passing
// for the wrong reason: with no limit the same query really writes every body.
func TestResponseLimitUnsetWritesEverything(t *testing.T) {
	f, e := newLimitExecutor(t, WithMaxConcurrency(4))
	resp := run(t, e, `{ a { items { body } } b { items { body } } }`, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
	if n := f.bodies.Load(); n != 2000 {
		t.Fatalf("wrote %d bodies with no limit, want 2000", n)
	}
}

// TestResponseLimitPerSubscriptionEvent pins that the limit is per event: one
// oversized event fails alone and the stream carries on.
func TestResponseLimitPerSubscriptionEvent(t *testing.T) {
	src, e := newSubExecutor(t, WithMaxResponseBytes(64))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id body } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	go func() {
		src.messages <- &subMessage{ID: "1", Body: strings.Repeat("x", 100)}
		src.messages <- &subMessage{ID: "2", Body: "ok"}
		close(src.messages)
	}()

	first := nextResponse(t, out)
	expectTooLarge(t, first, 64)
	first.Release()

	second := nextResponse(t, out)
	expectData(t, second, `{"messages":{"id":"2","body":"ok"}}`)
	second.Release()
	expectClosed(t, out)
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run TestResponseLimit .`
Expected: build failure, `undefined: WithMaxResponseBytes` and `undefined: CodeResponseTooLarge`.

- [ ] **Step 3: Implement.**

`errors.go`, inside the code `const` block after `CodeForbidden`:

```go
	// CodeResponseTooLarge is returned when a response's data exceeds
	// WithMaxResponseBytes.
	CodeResponseTooLarge = "RESPONSE_TOO_LARGE"
```

`limits.go`, after `WithMaxDepth`:

```go
// WithMaxResponseBytes bounds the size of each response's data: one query or
// mutation, or one subscription event. A response over the limit has null
// data and a single RESPONSE_TOO_LARGE error in place of any other errors,
// and execution stops resolving fields once it sees the limit passed.
//
// The decision on the finished response is exact. The bound while executing
// is not: a field is checked before it is written, so each buffer being
// written concurrently can pass the limit by about one field's output before
// execution notices. The errors list is not counted. Zero means unlimited,
// the default.
func WithMaxResponseBytes(n int64) ExecutorOption {
	return func(e *Executor) { e.maxResponseBytes = n }
}
```

`exec.go`: add to the `Executor` struct directly below the `maxDepth int` field:

```go
	maxResponseBytes int64
```

In `runOperation`, replace

```go
	w := jsonw.Get()
	st := &execState{e: e, vars: oc.Variables, decision: decision}
	ok := st.writeObject(ctx, w, p.root, p.sel, &Root{}, nil, p.op.Operation == ast.Mutation)
	if !ok {
		w.Reset()
		w.Null()
	}
```

with

```go
	w := e.newResponseWriter()
	st := &execState{e: e, vars: oc.Variables, decision: decision}
	ok := st.writeObject(ctx, w, p.root, p.sel, &Root{}, nil, p.op.Operation == ast.Mutation)
	st.finishData(ctx, w, ok)
```

and add below `runOperation`:

```go
// newResponseWriter returns the root writer for one response, carrying the
// executor's size limit when one is set.
func (e *Executor) newResponseWriter() *jsonw.Writer {
	w := jsonw.Get()
	if e.maxResponseBytes > 0 {
		w.Limit(e.maxResponseBytes)
	}
	return w
}

// finishData settles the root writer once every task has finished. The size
// check reads the budget before any Reset, which clears it. Over the limit,
// the field errors are dropped along with the data they point into.
func (st *execState) finishData(ctx context.Context, w *jsonw.Writer, ok bool) {
	if limit := st.e.maxResponseBytes; limit > 0 && (w.LimitExceeded() || int64(w.Len()) > limit) {
		w.Reset()
		w.Null()
		st.errs = nil
		st.addError(ctx, Errorf("response exceeds the maximum size of %d bytes", limit).WithCode(CodeResponseTooLarge), nil, nil)
		return
	}
	if !ok {
		w.Reset()
		w.Null()
	}
}
```

`subscription.go`, in `runSubscriptionEvent`, replace

```go
	w := jsonw.Get()
	st := &execState{e: e, vars: oc.Variables, decision: decision}
	if !st.writeObject(ctx, w, oc.plan.root, sel, &Root{}, nil, true) {
		w.Reset()
		w.Null()
	}
```

with

```go
	w := e.newResponseWriter()
	st := &execState{e: e, vars: oc.Variables, decision: decision}
	ok := st.writeObject(ctx, w, oc.plan.root, sel, &Root{}, nil, true)
	st.finishData(ctx, w, ok)
```

If `jsonw` is no longer referenced elsewhere in `subscription.go` the import stays: `f.exec.writeLeaf` still names `*jsonw.Writer`.

`exec_object.go`, in `writeFieldValue`, immediately before `if err := ctx.Err(); err != nil {`:

```go
	if w.OverLimit() {
		return false
	}
```

In `writeFieldsConcurrent` and in `writeListConcurrent`, directly after each `sub := jsonw.Get()`:

```go
			sub.ShareLimit(w)
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -count=1 -run 'TestResponseLimit|TestSubscribe' .`
Expected: `ok`.

- [ ] **Step 5: Deliberate breaks** — each must make the named test fail, then reverse the edit:
  1. Remove `sub.ShareLimit(w)` from `writeFieldsConcurrent` → `TestResponseLimitStopsSubWriters` fails.
  2. Remove the `if w.OverLimit() { return false }` checkpoint → `TestResponseLimitStopsSequentialExecution` fails.
  3. In `finishData`, delete `st.errs = nil` → `TestResponseLimitReplacesErrors` fails.
  4. In `finishData`, change `> limit` to `>= limit` → `TestResponseLimitExactBoundary` fails.
  5. In `runSubscriptionEvent`, use `jsonw.Get()` instead of `e.newResponseWriter()` → `TestResponseLimitPerSubscriptionEvent` fails.

- [ ] **Step 6: Full gate and size check**

Run: `go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`.
Run: `go test -run TestStructSizes -v . | grep -E "execState|OperationContext"` → `execState = 64 bytes`, `OperationContext = 160 bytes`.
Run: `go test -run '^$' -bench 'BenchmarkFieldPathBare$' -benchmem -count=1 .` → `18 allocs/op`.

- [ ] **Step 7: Commit**

```bash
git add errors.go limits.go exec.go exec_object.go subscription.go response_limit_test.go
git commit -m "feat: bound response data with WithMaxResponseBytes"
```

---

### Task 3: Decide the default by measurement, and document

Controller task (judgment; not dispatched).

**Files:**
- Create: `response_limit_bench_test.go`
- Modify: `exec.go` / `limits.go` (default, only if the measurement allows one), `CLAUDE.md`, `README.md`

- [ ] **Step 1: Benchmark pair** — create `response_limit_bench_test.go`:

```go
package graphql

import "testing"

// The pair measures what a response limit costs on the write path: one atomic
// report per field, and sharing across the concurrent sub-writers. The limit is
// far above the response, so both write identical bytes.
const responseLimitBenchQuery = `{ users { id name tags friends { id name tags friends { id name } } } }`

func BenchmarkResponseLimitOff(b *testing.B) {
	_, e := newFixtureExecutor(b)
	benchRun(b, e, responseLimitBenchQuery)
}

func BenchmarkResponseLimitOn(b *testing.B) {
	_, e := newFixtureExecutor(b, WithMaxResponseBytes(64<<20))
	benchRun(b, e, responseLimitBenchQuery)
}
```

Verify first that the query returns data without errors under both executors (a one-off run in a test or by printing `resp.Data` once), so the benchmark is not timing an error path.

- [ ] **Step 2: Interleaved measurement** — build once, alternate 12 times, compare:

```bash
go test -c -o $SCRATCH/limit.exe .
for i in $(seq 1 12); do
  $SCRATCH/limit.exe -test.run xxx -test.bench 'BenchmarkResponseLimitOff$' -test.benchmem -test.count=1 >> $SCRATCH/off.txt
  $SCRATCH/limit.exe -test.run xxx -test.bench 'BenchmarkResponseLimitOn$'  -test.benchmem -test.count=1 >> $SCRATCH/on.txt
done
sed -i 's/BenchmarkResponseLimitOn/BenchmarkResponseLimit/' $SCRATCH/on.txt
sed -i 's/BenchmarkResponseLimitOff/BenchmarkResponseLimit/' $SCRATCH/off.txt
benchstat $SCRATCH/off.txt $SCRATCH/on.txt
```

Also run the no-limit cost against `main` for the disabled path: build `main`'s test binary from a detached worktree and alternate `BenchmarkFieldPathBare$` 12 times each.

- [ ] **Step 3: Decide.** If the enabled delta is not statistically significant (`~` in benchstat) and allocations are equal: set the default to 64 MiB in `NewExecutor` (`maxResponseBytes: 64 << 20`), change the option doc's last sentence to "Zero means unlimited. The default is 64 MiB.", and add `TestExecutorResponseLimitDefault` asserting `NewExecutor(schema).maxResponseBytes == 64<<20`. Otherwise keep the default at zero. Either way record the numbers.

- [ ] **Step 4: Docs.** `CLAUDE.md`: in the Execute section, a paragraph on the budget living on the writer (why not `execState`), per-checkpoint reporting (why not blocks), exactness at the end vs. in-flight overshoot, the measured cost and the default decision, and that `errors` is not bounded; update the Status line. `README.md`: add the option wherever the other executor limits are listed.

- [ ] **Step 5: Gate and commit**

Run: `sh scripts/gate.sh`
Expected: every module `ok`.

```bash
git add response_limit_bench_test.go CLAUDE.md README.md exec.go limits.go response_limit_test.go
git commit -m "docs: record the response limit's measured cost and its default"
```
