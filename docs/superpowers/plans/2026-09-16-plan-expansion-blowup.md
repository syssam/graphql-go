# Plan Expansion Blowup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound plan compilation so that a small query selecting an abstract type deeply cannot build `N^depth` selection sets, and make `WithMaxDepth`/`WithMaxComplexity` reject such a query before the expansion instead of after it.

**Architecture:** `compileSelection` is memoized on `(parent type, selection set)`, which collapses the expansion tree into a DAG of shared, read-only selection sets. The three walks over that structure (`complexityOf`, `depthOf`, `queryCostOf`) are memoized so they do not re-walk shared subgraphs. The depth/complexity computation then moves off the plan tree onto the document AST so it runs before compilation, with the old plan-tree walk retained in a test file as a differential oracle.

**Tech Stack:** Go 1.27, `github.com/vektah/gqlparser/v2` (AST and validator). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-16-plan-expansion-blowup-design.md`

## Global Constraints

- **Root package may depend only on `gqlparser/v2` and the standard library.** Every file in this plan is in the root package; add no imports outside that set.
- **No reflection on the request hot path.** Plan compile is not the hot path and may use reflection; `queryCostOf` runs per request and must not.
- **No `unsafe`, no `uintptr`.** AST node identity is expressed through the comparable `ast.Selection` interface value, never its address.
- **Go 1.27 minimum.**
- **Comments explain why, not what. English only. No code-narrating comments.**
- **Tests live beside the code in `package graphql`.**
- **Commit messages: imperative, lower-case type prefix** (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`).
- **The gate is `sh scripts/gate.sh`**, not `go test ./...` — this repository has four modules and `go test ./...` reaches one.
- **`-race` is not optional** on any test run in this plan.

---

### Task 1: Pin the duplicate compile-error behaviour

Memoizing `compileSelection` will collapse a plan-time argument error that currently fires once per concrete type into a single error. Spec §7 requires pinning the current behaviour first, so the change in Task 2 is deliberate and visible in a diff rather than discovered later.

**Files:**
- Create: `plan_duperr_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: nothing later tasks depend on. This test is modified by Task 2 and then left alone.

- [ ] **Step 1: Write the test that pins today's behaviour**

Create `plan_duperr_test.go`. The fixture needs a plan-time-only argument failure: a custom scalar whose `unmarshal` rejects the literal. gqlparser accepts any literal for a custom scalar, so validation passes and the failure lands in `buildField` at `plan.go:317`.

```go
package graphql

import (
	"fmt"
	"testing"
)

// dupErrSDL puts an argument whose scalar fails to decode on an interface
// field, so every concrete type's expansion of that field hits the same
// failure. Three types are enough to observe duplication.
const dupErrSDL = `
scalar Weekday
interface Node { id: ID! at(day: Weekday!): String }
type D0 implements Node { id: ID! at(day: Weekday!): String }
type D1 implements Node { id: ID! at(day: Weekday!): String }
type D2 implements Node { id: ID! at(day: Weekday!): String }
type Query { root: Node }
`

type weekday string

type dupTag0 struct{}
type dupTag1 struct{}
type dupTag2 struct{}

type dupT[T any] struct{ ID string }

func (d *dupT[T]) dupNodeID() string { return d.ID }

type dupNode interface{ dupNodeID() string }

type dayArgs struct {
	Day weekday
}

func dupObject[T any](name string) SchemaOption {
	return Object[dupT[T]](name,
		Field("id", func(d *dupT[T]) string { return d.ID }),
		FieldArgs("at", func(d *dupT[T], a dayArgs) string { return string(a.Day) }),
	)
}

func newDupErrSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(dupErrSDL),
		Scalar("Weekday",
			func(w *Writer, v weekday) error { w.String(string(v)); return nil },
			func(v any) (weekday, error) {
				s, ok := v.(string)
				if !ok || s == "Funday" {
					return "", fmt.Errorf("not a weekday: %v", v)
				}
				return weekday(s), nil
			}),
		Args[dayArgs](),
		dupObject[dupTag0]("D0"),
		dupObject[dupTag1]("D1"),
		dupObject[dupTag2]("D2"),
		Object[Root]("Query", Field("root", func(Root) dupNode { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPlanArgErrorPerConcreteType pins how many errors one bad literal
// produces when it sits under an abstract parent. Today compileSelection
// expands the selection once per concrete type, so the same failure is
// reported three times.
func TestPlanArgErrorPerConcreteType(t *testing.T) {
	s := newDupErrSchema(t)
	e := NewExecutor(s)
	entry, errs := e.document(`{ root { at(day: "Funday") } }`)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	_, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if len(perrs) != 3 {
		t.Fatalf("got %d errors, want 3 (one per concrete type)", len(perrs))
	}
}
```

- [ ] **Step 2: Run the test and confirm it passes**

Run: `go test -race -count=1 -run TestPlanArgErrorPerConcreteType .`
Expected: PASS. If it fails with a different count, the count in the assertion is what today actually does — update the assertion to the observed number and note it in the commit message. Do not change production code in this task.

If it fails because `Writer.String` does not exist or `Scalar`'s marshal signature differs, check `scalar.go:19` and the existing custom scalar tests in `scalar_test.go` and match them. The assertion on error count is the point of the test; the fixture spelling is not.

- [ ] **Step 3: Commit**

```bash
git add plan_duperr_test.go
git commit -m "test: pin per-concrete-type duplication of plan argument errors"
```

---

### Task 2: Memoize plan expansion

**Files:**
- Create: `plan_fanout_test.go`
- Modify: `plan.go` — `compiler` struct (~line 95), `compilePlan` (~line 107), `compileSelection` (line 139), `complexityOf` (line 365)
- Modify: `limits.go` — `depthOf` (line 220)
- Modify: `plan_duperr_test.go` (from Task 1)

**Interfaces:**
- Consumes: nothing from Task 1 except the file it edits.
- Produces, used by Tasks 3-6:
  - `fanTypes` — untyped constant `8`, the number of concrete types in the fan-out fixture.
  - `fanSDL() string`
  - `fanQuery(depth int) string`
  - `newFanExecutor(t *testing.T, opts ...ExecutorOption) (*Schema, *Executor)`
  - `fanPlan(t *testing.T, s *Schema, e *Executor, depth int) *plan`
  - `countSelectionSets(sel *selectionSet) int`
  - `(*compiler).selFingerprint(sels ast.SelectionSet) string`
  - `selKey` struct with fields `parent any`, `sels string`

- [ ] **Step 1: Write the fan-out fixture and the failing bound test**

Create `plan_fanout_test.go`. The fixture uses one generic struct instantiated at distinct tag types, so eight schema types need one struct declaration and one method rather than sixteen declarations. Distinct instantiations of a generic type are distinct `reflect.Type` values, which is what the registry keys on.

```go
package graphql

import (
	"fmt"
	"strings"
	"testing"
)

// fanTagN give fanT distinct instantiations. The registry keys bindings by
// (GraphQL type name, reflect.Type), so the eight schema types need eight Go
// types; generic instantiation supplies them without eight declarations.
type (
	fanTag0 struct{}
	fanTag1 struct{}
	fanTag2 struct{}
	fanTag3 struct{}
	fanTag4 struct{}
	fanTag5 struct{}
	fanTag6 struct{}
	fanTag7 struct{}
)

const fanTypes = 8

type fanT[T any] struct{ ID string }

func (t *fanT[T]) fanNodeID() string { return t.ID }

type fanNode interface{ fanNodeID() string }

// fanSDL is cyclic through the abstract type: Node.next returns Node, so a
// planner that expands every possible type at every level does fanTypes^depth
// work for a query that only ever resolves one concrete type per level.
func fanSDL() string {
	var b strings.Builder
	b.WriteString("interface Node { id: ID! next: Node }\n")
	for i := 0; i < fanTypes; i++ {
		fmt.Fprintf(&b, "type T%d implements Node { id: ID! next: Node }\n", i)
	}
	b.WriteString("type Query { root: Node }\n")
	return b.String()
}

func fanObject[T any](name string) SchemaOption {
	return Object[fanT[T]](name,
		Field("id", func(t *fanT[T]) string { return t.ID }),
		Field("next", func(t *fanT[T]) fanNode { return nil }),
	)
}

func newFanExecutor(t *testing.T, opts ...ExecutorOption) (*Schema, *Executor) {
	t.Helper()
	s, err := NewSchema(SDL(fanSDL()),
		fanObject[fanTag0]("T0"),
		fanObject[fanTag1]("T1"),
		fanObject[fanTag2]("T2"),
		fanObject[fanTag3]("T3"),
		fanObject[fanTag4]("T4"),
		fanObject[fanTag5]("T5"),
		fanObject[fanTag6]("T6"),
		fanObject[fanTag7]("T7"),
		Object[Root]("Query", Field("root", func(Root) fanNode { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s, NewExecutor(s, opts...)
}

// fanQuery nests `next` depth levels below root.
func fanQuery(depth int) string {
	var b strings.Builder
	b.WriteString("{ root { id ")
	for i := 0; i < depth; i++ {
		b.WriteString("next { id ")
	}
	b.WriteString(strings.Repeat("} ", depth))
	b.WriteString("} }")
	return b.String()
}

func fanPlan(t *testing.T, s *Schema, e *Executor, depth int) *plan {
	t.Helper()
	entry, errs := e.document(fanQuery(depth))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	p, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs[0])
	}
	return p
}

// countSelectionSets counts selection sets by pointer identity, so a memoized
// compile that shares one set across many paths counts it once.
func countSelectionSets(sel *selectionSet) int {
	seen := make(map[*selectionSet]bool)
	var walk func(*selectionSet)
	walk = func(s *selectionSet) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		for _, sub := range s.byType {
			walk(sub)
		}
		for _, f := range s.fields {
			walk(f.sub)
		}
	}
	walk(sel)
	return len(seen)
}

// TestFanOutExpansionIsBounded is the regression test for the plan blowup.
// Counting selection sets rather than timing the compile keeps it
// deterministic: the bound is a property of the algorithm, not of the machine.
//
// Before memoization this compiles fanTypes^6 = 262,144 selection sets.
func TestFanOutExpansionIsBounded(t *testing.T) {
	s, e := newFanExecutor(t)
	const depth = 6
	p := fanPlan(t, s, e, depth)
	got := countSelectionSets(p.sel)
	// One abstract set and one concrete set per type, per level, with slack
	// for the root and the query type.
	want := fanTypes * (depth + 2) * 2
	if got > want {
		t.Fatalf("plan holds %d selection sets, want at most %d", got, want)
	}
}

// TestFanOutMemoSurvivesDuplicateKeys guards the memo's key. buildField reuses
// first.SelectionSet when a response key has one AST node but appends a fresh
// slice when it has more, so a key derived from slice identity would miss at
// every level here and the expansion would be exponential again.
func TestFanOutMemoSurvivesDuplicateKeys(t *testing.T) {
	s, e := newFanExecutor(t)
	var b strings.Builder
	b.WriteString("{ root { ")
	const depth = 5
	for i := 0; i < depth; i++ {
		// Two selections on the same response key force buildField's merge
		// branch, which allocates a new slice.
		b.WriteString("next { id } next { ")
	}
	b.WriteString("id ")
	b.WriteString(strings.Repeat("} ", depth))
	b.WriteString("} }")

	entry, errs := e.document(b.String())
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	p, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs[0])
	}
	got := countSelectionSets(p.sel)
	want := fanTypes * (depth + 2) * 4
	if got > want {
		t.Fatalf("plan holds %d selection sets, want at most %d", got, want)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test -race -count=1 -run 'TestFanOut' .`
Expected: both FAIL, reporting a selection-set count in the hundreds of thousands against a bound in the low hundreds. `TestFanOutExpansionIsBounded` will take a second or two before failing; that slowness is the bug.

- [ ] **Step 3: Add the memo to the compiler**

In `plan.go`, extend `compiler` (currently ending at the `errs` field around line 100) and add the key type and fingerprint helpers:

```go
type compiler struct {
	s    *Schema
	e    *Executor
	doc  *ast.QueryDocument
	cond map[string]bool
	errs []*Error

	// An interface with N implementers selected D levels deep expands to N^D
	// selection sets, because each concrete expansion can contain further
	// abstract fields. compileSelection is a pure function of its parent and
	// selection set for a fixed cond, and plan structures are read-only once
	// built, so identical calls share one result and the tree becomes a DAG.
	memo   map[selKey]*selectionSet
	nodeID map[ast.Selection]int32
}

// selKey identifies a compileSelection call. parent holds the *objectType for
// a concrete parent or the *abstractType for an abstract one; both are
// pointers, so the interface value is comparable.
type selKey struct {
	parent any
	sels   string
}

// selFingerprint identifies a selection set by the identities of the AST nodes
// in it. Slice identity will not do: buildField reuses first.SelectionSet when
// a response key has a single AST node but appends a fresh slice when it has
// more, so a query repeating a response key would miss a pointer-keyed memo at
// every level and expand exponentially regardless.
func (c *compiler) selFingerprint(sels ast.SelectionSet) string {
	var b []byte
	for _, s := range sels {
		b = strconv.AppendInt(b, int64(c.nodeNum(s)), 36)
		b = append(b, ',')
	}
	return string(b)
}

// nodeNum numbers an AST selection node on first sight. The document is cached
// and never mutated, so a node's identity is stable across every plan compiled
// from it.
func (c *compiler) nodeNum(s ast.Selection) int32 {
	if n, ok := c.nodeID[s]; ok {
		return n
	}
	n := int32(len(c.nodeID))
	c.nodeID[s] = n
	return n
}
```

Add `"strconv"` to `plan.go`'s imports.

- [ ] **Step 4: Initialize the memo in compilePlan**

In `compilePlan` (`plan.go:107`), replace the compiler construction:

```go
	c := &compiler{
		s: s, e: e, doc: doc, cond: cond,
		memo:   make(map[selKey]*selectionSet),
		nodeID: make(map[ast.Selection]int32),
	}
```

- [ ] **Step 5: Consult the memo in compileSelection**

Rewrite `compileSelection` (`plan.go:139`) so both the concrete and abstract paths go through the memo. The body is otherwise unchanged.

```go
func (c *compiler) compileSelection(obj *objectType, abs *abstractType, sels ast.SelectionSet) *selectionSet {
	key := selKey{parent: any(obj), sels: c.selFingerprint(sels)}
	if abs != nil {
		key.parent = any(abs)
	}
	if s, ok := c.memo[key]; ok {
		return s
	}

	out := &selectionSet{}
	if abs == nil {
		out.fields = c.collect(obj, sels)
		out.directSchedulable, out.deepSchedulable = schedulability(out.fields)
		c.memo[key] = out
		return out
	}
	names := make([]string, 0, len(abs.possible))
	for name := range abs.possible {
		names = append(names, name)
	}
	sort.Strings(names)
	out.byType = make(map[string]*selectionSet, len(names))
	for _, name := range names {
		concrete := c.compileSelection(abs.possible[name], nil, sels)
		out.byType[name] = concrete
		out.directSchedulable = max(out.directSchedulable, concrete.directSchedulable)
		out.deepSchedulable = out.deepSchedulable || concrete.deepSchedulable
	}
	c.memo[key] = out
	return out
}
```

Store after the body completes, not before: a selection set's children are compiled from strictly smaller AST selection sets and `collect` starts each traversal with a fresh `visited` map, so the recursion terminates and no entry is ever read while incomplete.

- [ ] **Step 6: Memoize complexityOf**

The plan is now a DAG, so a plain recursive walk re-visits shared subgraphs exponentially and compile stays slow even though the plan is small. Replace `complexityOf` (`plan.go:365`):

```go
func complexityOf(sel *selectionSet) int {
	return complexityMemo(sel, make(map[*selectionSet]int))
}

// complexityMemo carries the memo that makes the walk linear in the DAG rather
// than in the tree the DAG unfolds to.
func complexityMemo(sel *selectionSet, memo map[*selectionSet]int) int {
	if sel == nil {
		return 0
	}
	if n, ok := memo[sel]; ok {
		return n
	}
	count := func(fields []*planField) int {
		n := 0
		for _, f := range fields {
			n += 1 + complexityMemo(f.sub, memo)
		}
		return n
	}
	n := 0
	if sel.byType == nil {
		n = count(sel.fields)
	} else {
		for _, concrete := range sel.byType {
			n = max(n, count(concrete.fields))
		}
	}
	memo[sel] = n
	return n
}
```

- [ ] **Step 7: Memoize depthOf**

Replace `depthOf` (`limits.go:220`) the same way:

```go
func depthOf(sel *selectionSet) int {
	return depthMemo(sel, make(map[*selectionSet]int))
}

func depthMemo(sel *selectionSet, memo map[*selectionSet]int) int {
	if sel == nil {
		return 0
	}
	if n, ok := memo[sel]; ok {
		return n
	}
	walk := func(fields []*planField) int {
		d := 0
		for _, f := range fields {
			fd := 1
			if f.sub != nil {
				fd += depthMemo(f.sub, memo)
			}
			d = max(d, fd)
		}
		return d
	}
	d := 0
	if sel.byType == nil {
		d = walk(sel.fields)
	} else {
		for _, c := range sel.byType {
			d = max(d, walk(c.fields))
		}
	}
	memo[sel] = d
	return d
}
```

- [ ] **Step 8: Run the fan-out tests and verify they pass**

Run: `go test -race -count=1 -run 'TestFanOut' .`
Expected: both PASS, in well under a second.

- [ ] **Step 9: Verify the test fails when the memo is removed**

A regression test that passes against broken code is agreeing with the code, not checking it. Temporarily comment out the two `c.memo[key] = out` assignments in `compileSelection`.

Run: `go test -race -count=1 -run TestFanOutExpansionIsBounded .`
Expected: FAIL, reporting roughly 262,144 selection sets.

Then comment out only the memo *read* (`if s, ok := c.memo[key]; ok`) and leave the writes, and run it again. Expected: FAIL as well.

Restore both. Re-run and confirm PASS. Do not commit with either edit in place.

- [ ] **Step 10: Update the duplicate-error test from Task 1**

Memoization collapses the three identical errors into one. Change the assertion in `plan_duperr_test.go` and rewrite the doc comment to record why:

```go
// TestPlanArgErrorPerConcreteType pins how many errors one bad literal
// produces when it sits under an abstract parent. compileSelection memoizes on
// (parent type, selection set), so the shared failing selection is compiled —
// and reported — once rather than once per concrete type.
func TestPlanArgErrorPerConcreteType(t *testing.T) {
	s := newDupErrSchema(t)
	e := NewExecutor(s)
	entry, errs := e.document(`{ root { at(day: "Funday") } }`)
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	_, _, perrs := entry.planFor(s, e, entry.doc.Operations[0], nil)
	if len(perrs) != 1 {
		t.Fatalf("got %d errors, want 1", len(perrs))
	}
}
```

- [ ] **Step 11: Run the full root-package suite**

Run: `go vet ./... && go test -race -count=1 .`
Expected: PASS. Existing tests assert `complexity` and `depth` values; none should change, because memoization shares results without altering them. If any does change, stop — that means the memo key is conflating selections it should not, and the key is wrong.

- [ ] **Step 12: Commit**

```bash
git add plan.go limits.go plan_fanout_test.go plan_duperr_test.go
git commit -m "fix: memoize plan expansion so abstract selections cannot blow up

compileSelection expanded an abstract parent into one concrete selection set
per possible type, and each expansion could contain further abstract fields,
so an interface with N implementers selected D deep built N^D selection sets:
20.2s and 42.3M selection sets for 12 types at depth 6, from a 200-byte query.

Memoize on (parent type, selection set). The call is a pure function of those
for a fixed skip/include variant and plan structures are read-only once built,
so the expansion tree collapses to a DAG of shared sets. complexityOf and
depthOf memoize too, or they walk the DAG as if it were still the tree."
```

---

### Task 3: Memoize per-request query cost

`queryCostOf` (`limits.go:141`) runs per request from the operation interceptor chain (`interceptor.go:136`) and depends on `oc.Variables`, so it cannot be cached on the plan. On the DAG it must memoize per call or it walks the unfolded tree on every request.

**Files:**
- Modify: `limits.go` — `queryCostOf` (line 141), `fieldCost` (line 162)
- Modify: `plan_fanout_test.go`

**Interfaces:**
- Consumes: `newFanExecutor`, `fanQuery`, `fanTypes` from Task 2.
- Produces: `queryCostOf(sel *selectionSet, vars map[string]any, cfg QueryCost) int` keeps its exported-to-package signature and behaviour; `fieldCost` gains a trailing `memo map[*selectionSet]int` parameter.

- [ ] **Step 1: Write the failing test**

Append to `plan_fanout_test.go`:

```go
// TestFanOutQueryCostIsBounded covers the walk that runs per request rather
// than per compile. queryCostOf depends on request variables, so its result
// cannot live on the plan; without its own memo it re-walks every shared
// subgraph of the DAG on every request.
func TestFanOutQueryCostIsBounded(t *testing.T) {
	s, e := newFanExecutor(t, WithQueryCost(QueryCost{Report: true}))
	const depth = 6
	p := fanPlan(t, s, e, depth)

	visits := 0
	var walk func(*selectionSet)
	walk = func(sel *selectionSet) {
		if sel == nil {
			return
		}
		visits++
		for _, sub := range sel.byType {
			walk(sub)
		}
		for _, f := range sel.fields {
			walk(f.sub)
		}
	}
	walk(p.sel)
	// An unmemoized walk of the DAG unfolds it back to the tree. If that is
	// still cheap the fixture is too small to be measuring anything.
	if visits < 10000 {
		t.Fatalf("fixture unfolds to only %d visits; it cannot detect the regression", visits)
	}

	start := time.Now()
	for i := 0; i < 50; i++ {
		if n := queryCostOf(p.sel, nil, QueryCost{}); n <= 0 {
			t.Fatalf("cost = %d, want positive", n)
		}
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("50 cost walks took %v; the walk is not memoized", d)
	}
}
```

Add `"time"` to `plan_fanout_test.go`'s imports.

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test -race -count=1 -run TestFanOutQueryCostIsBounded .`
Expected: FAIL on the duration assertion. The `visits` guard should pass; if it fails instead, the DAG unfolds to fewer than 10,000 nodes and the fixture depth must be raised until it does not.

- [ ] **Step 3: Thread a memo through the cost walk**

Replace `queryCostOf` and `fieldCost` in `limits.go`. The memo is created per call because `vars` and `cfg` are fixed only within one request.

```go
func queryCostOf(sel *selectionSet, vars map[string]any, cfg QueryCost) int {
	return queryCostMemo(sel, vars, cfg, make(map[*selectionSet]int))
}

func queryCostMemo(sel *selectionSet, vars map[string]any, cfg QueryCost, memo map[*selectionSet]int) int {
	if sel == nil {
		return 0
	}
	if n, ok := memo[sel]; ok {
		return n
	}
	sum := func(fields []*planField) int {
		n := 0
		for _, f := range fields {
			n += fieldCost(f, vars, cfg, memo)
		}
		return n
	}
	n := 0
	if sel.byType == nil {
		n = sum(sel.fields)
	} else {
		for _, concrete := range sel.byType {
			n = max(n, sum(concrete.fields))
		}
	}
	memo[sel] = n
	return n
}
```

In `fieldCost`, add the trailing parameter and pass it down:

```go
func fieldCost(f *planField, vars map[string]any, cfg QueryCost, memo map[*selectionSet]int) int {
```

and change its recursive call:

```go
	child := 0
	if f.sub != nil {
		child = queryCostMemo(f.sub, vars, cfg, memo)
	}
```

Leave the rest of `fieldCost` untouched.

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test -race -count=1 -run TestFanOutQueryCostIsBounded .`
Expected: PASS.

- [ ] **Step 5: Verify the test fails without the memo**

Temporarily change `queryCostMemo`'s early return to always miss by commenting out the `if n, ok := memo[sel]; ok` block.

Run: `go test -race -count=1 -run TestFanOutQueryCostIsBounded .`
Expected: FAIL on the duration assertion. Restore, re-run, confirm PASS.

- [ ] **Step 6: Run the cost and limits suites**

Run: `go vet ./... && go test -race -count=1 -run 'Cost|Limit|Complexity|Depth' .`
Expected: PASS, with `extensions.cost` values unchanged. Memoization must not alter any reported number.

- [ ] **Step 7: Commit**

```bash
git add limits.go plan_fanout_test.go
git commit -m "fix: memoize the per-request query cost walk

queryCostOf depends on request variables, so its result cannot live on the
plan and it runs once per request. With the plan now a DAG, an unmemoized walk
unfolds it back to the tree on every request."
```

---

### Task 4: Compute depth and complexity from the document AST

The guard has to run before `compilePlan`, so the walk has to work on the AST. Spec §5.1 keeps one implementation: this one replaces the plan-tree walks, which move to a test file as the differential oracle in Task 5.

The walk mirrors `compileSelection` and `collect` exactly — including the memo, without which the guard itself would blow up on the query it is meant to reject.

**Files:**
- Create: `plan_metrics.go`
- Create: `plan_metrics_test.go`

**Interfaces:**
- Consumes: `compiler`, `selKey`, `(*compiler).selFingerprint`, `(*compiler).nodeNum`, `(*compiler).collectInto`, `(*compiler).applies`, `(*compiler).included`, `(*Schema).rootFor` — all from `plan.go` after Task 2.
- Produces, used by Task 5:
  - `type planMetrics struct { complexity, depth int }`
  - `func operationMetrics(s *Schema, doc *ast.QueryDocument, op *ast.OperationDefinition, cond map[string]bool) planMetrics`

- [ ] **Step 1: Write the failing equivalence test**

Create `plan_metrics_test.go`. It compares the new walk against the plan compiled by the existing code, so it pins equivalence before the old walk is retired.

```go
package graphql

import "testing"

// metricsCases covers the constructs where an AST walk and a plan-tree walk
// can disagree: abstract parents taking a max over concrete types, duplicate
// response keys merging, fragments, and skip/include folding.
var metricsCases = []struct {
	name  string
	query string
}{
	{"flat", `{ root { id } }`},
	{"nested", `{ root { id next { id next { id } } } }`},
	{"duplicate keys", `{ root { next { id } next { id } } }`},
	{"inline fragment", `{ root { ... on T0 { id } ... on T1 { id next { id } } } }`},
	{"named fragment", `fragment F on Node { id next { id } } { root { ...F } }`},
	{"typename", `{ root { __typename next { __typename id } } }`},
	{"skip literal", `{ root { id @skip(if: true) next { id } } }`},
	{"include literal", `{ root { id @include(if: false) next { id } } }`},
}

func TestOperationMetricsMatchesPlan(t *testing.T) {
	s, e := newFanExecutor(t)
	for _, tc := range metricsCases {
		t.Run(tc.name, func(t *testing.T) {
			entry, errs := e.document(tc.query)
			if errs != nil {
				t.Fatalf("document: %v", errs[0])
			}
			op := entry.doc.Operations[0]
			p, _, perrs := entry.planFor(s, e, op, nil)
			if perrs != nil {
				t.Fatalf("plan: %v", perrs[0])
			}
			got := operationMetrics(s, entry.doc, op, nil)
			if got.complexity != p.complexity {
				t.Errorf("complexity = %d, plan says %d", got.complexity, p.complexity)
			}
			if got.depth != p.depth {
				t.Errorf("depth = %d, plan says %d", got.depth, p.depth)
			}
		})
	}
}

// TestOperationMetricsIsBounded is the reason the walk carries its own memo:
// a guard that expands N^depth to decide whether to refuse to expand N^depth
// has refused nothing.
func TestOperationMetricsIsBounded(t *testing.T) {
	s, e := newFanExecutor(t)
	entry, errs := e.document(fanQuery(9))
	if errs != nil {
		t.Fatalf("document: %v", errs[0])
	}
	// Depth 9 is fanTypes^9 unfolded; if this returns at all, the walk is
	// memoized. The plan is deliberately never compiled here.
	if m := operationMetrics(s, entry.doc, entry.doc.Operations[0], nil); m.depth != 10 {
		t.Fatalf("depth = %d, want 10", m.depth)
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test -race -count=1 -run TestOperationMetrics .`
Expected: FAIL to compile — `operationMetrics` is undefined.

- [ ] **Step 3: Implement the walk**

Create `plan_metrics.go`:

```go
package graphql

import "github.com/vektah/gqlparser/v2/ast"

// planMetrics is an operation's static complexity and nesting depth.
type planMetrics struct {
	complexity int
	depth      int
}

// operationMetrics computes an operation's complexity and depth from the
// document, before any plan exists. Computing them from the finished plan, as
// this once did, means the limits can only report on an expansion that has
// already happened rather than prevent it.
//
// It must agree exactly with the plan-tree walk it replaces; that walk is
// retained as planMetricsOracle in plan_metrics_oracle_test.go and the two are
// compared by TestOperationMetricsMatchesPlan and FuzzOperationMetrics.
func operationMetrics(s *Schema, doc *ast.QueryDocument, op *ast.OperationDefinition, cond map[string]bool) planMetrics {
	root := s.rootFor(op.Operation)
	if root == nil {
		return planMetrics{}
	}
	w := &metricWalker{
		c:    &compiler{s: s, doc: doc, cond: cond, nodeID: make(map[ast.Selection]int32)},
		memo: make(map[selKey]planMetrics),
	}
	return w.walk(root, nil, op.SelectionSet)
}

// metricWalker mirrors compileSelection without building a plan. It carries
// the same memo, because a guard that expands N^depth in order to decide
// whether to refuse expanding N^depth has refused nothing.
type metricWalker struct {
	c    *compiler
	memo map[selKey]planMetrics
}

func (w *metricWalker) walk(obj *objectType, abs *abstractType, sels ast.SelectionSet) planMetrics {
	key := selKey{parent: any(obj), sels: w.c.selFingerprint(sels)}
	if abs != nil {
		key.parent = any(abs)
	}
	if m, ok := w.memo[key]; ok {
		return m
	}

	var m planMetrics
	if abs == nil {
		m = w.walkConcrete(obj, sels)
	} else {
		// An abstract parent contributes the largest of its possible types,
		// matching how the plan-tree walk takes a max over byType.
		for _, concrete := range abs.possible {
			c := w.walkConcrete(concrete, sels)
			m.complexity = max(m.complexity, c.complexity)
			m.depth = max(m.depth, c.depth)
		}
	}
	w.memo[key] = m
	return m
}

func (w *metricWalker) walkConcrete(obj *objectType, sels ast.SelectionSet) planMetrics {
	var groups []*fieldGroup
	index := make(map[string]*fieldGroup)
	visited := make(map[string]bool)
	w.c.collectInto(obj, sels, &groups, index, visited)

	var m planMetrics
	for _, g := range groups {
		first := g.fields[0]
		if first.Name == "__typename" {
			m.complexity++
			m.depth = max(m.depth, 1)
			continue
		}
		fd := obj.fields[first.Name]
		if fd == nil {
			// buildField drops this group and reports an error, so it
			// contributes nothing to either number.
			continue
		}
		if fd.leaf {
			m.complexity++
			m.depth = max(m.depth, 1)
			continue
		}

		var merged ast.SelectionSet
		if len(g.fields) == 1 {
			merged = first.SelectionSet
		} else {
			for _, f := range g.fields {
				merged = append(merged, f.SelectionSet...)
			}
		}
		named := w.c.s.ast.Types[fd.typ.Name()]
		var sub planMetrics
		if named.Kind == ast.Object {
			sub = w.walk(w.c.s.objects[named.Name], nil, merged)
		} else {
			sub = w.walk(nil, w.c.s.abstracts[named.Name], merged)
		}
		m.complexity += 1 + sub.complexity
		m.depth = max(m.depth, 1+sub.depth)
	}
	return m
}
```

The `merged` construction is copied from `buildField` (`plan.go:327`) rather than shared, because the two diverge in what they do next and a shared helper would have to return both. If `buildField`'s merge rule changes, this must change with it — `TestOperationMetricsMatchesPlan` is what catches it.

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test -race -count=1 -run TestOperationMetrics .`
Expected: PASS on every subtest.

If a subtest disagrees, the plan-tree walk is the specification and this walk is wrong. Read `complexityOf` (`plan.go:365`) and `depthMemo` (`limits.go:220`) against the failing case and fix `plan_metrics.go`. Do not adjust the plan-tree walk to match.

- [ ] **Step 5: Commit**

```bash
git add plan_metrics.go plan_metrics_test.go
git commit -m "feat: compute operation depth and complexity from the document AST

Computing them from the finished plan means the limits can only report on an
expansion that has already happened. This walk mirrors compileSelection
without building a plan, and carries the same memo, so it can run before
compilation. It is not wired up yet."
```

---

### Task 5: Make the limits a guard

**Files:**
- Modify: `plan.go` — `compilePlan` (line 107, drop the metric calls), `docEntry.planFor` (line 447)
- Modify: `limits.go` — add the pre-compile check, remove `depthOf`/`depthMemo`
- Create: `plan_metrics_oracle_test.go` — the retired plan-tree walks
- Modify: `plan_fanout_test.go` — the guard test

**Interfaces:**
- Consumes: `operationMetrics`, `planMetrics` from Task 4; `fanQuery`, `newFanExecutor` from Task 2.
- Produces, used by Task 6:
  - `func (e *Executor) rejectByMetrics(m planMetrics) *Error`
  - `func planMetricsOracle(p *plan) planMetrics` in `plan_metrics_oracle_test.go`

- [ ] **Step 1: Write the failing guard test**

Append to `plan_fanout_test.go`:

```go
// TestDepthLimitRejectsBeforeCompiling is the point of the guard. Before it,
// WithMaxDepth(3) against this query still built 8^5 selection sets and only
// then reported that the query was three levels too deep.
func TestDepthLimitRejectsBeforeCompiling(t *testing.T) {
	s, e := newFanExecutor(t, WithMaxDepth(3))
	_ = s

	start := time.Now()
	resp := e.Execute(t.Context(), &Request{Query: fanQuery(8)})
	elapsed := time.Since(start)

	if len(resp.Errors) == 0 {
		t.Fatal("want a depth limit error")
	}
	if got := resp.Errors[0].Message; !strings.Contains(got, "maximum depth") {
		t.Fatalf("error = %q, want a maximum depth error", got)
	}
	// Depth 8 is 16 million selection sets unmemoized and tens of thousands
	// memoized. Rejecting without compiling should be neither.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("rejection took %v; the query was compiled before being refused", elapsed)
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test -race -count=1 -run TestDepthLimitRejectsBeforeCompiling .`
Expected: FAIL on the duration assertion. The error message assertion should already pass — the limit works today, it is only late.

- [ ] **Step 3: Add the metric check to the executor**

In `limits.go`, add beside `rejectIfOverLimit`:

```go
// rejectByMetrics applies the static limits to metrics computed before the
// plan is compiled. rejectIfOverLimit applies the same two limits per request
// from the cached plan; this one exists so that a query over the limit is
// never compiled at all.
func (e *Executor) rejectByMetrics(m planMetrics) *Error {
	if e.maxComplexity > 0 && m.complexity > e.maxComplexity {
		return Errorf("query exceeds complexity limit: %d > %d", m.complexity, e.maxComplexity).
			WithCode(CodeTooComplex).
			WithExtension("complexity", m.complexity).
			WithExtension("maxComplexity", e.maxComplexity)
	}
	if e.maxDepth > 0 && m.depth > e.maxDepth {
		return Errorf("query exceeds maximum depth: %d > %d", m.depth, e.maxDepth).
			WithCode(CodeMaxDepth).
			WithExtension("depth", m.depth).
			WithExtension("maxDepth", e.maxDepth)
	}
	return nil
}
```

The messages, codes and extension keys are copied from `rejectIfOverLimit` verbatim so a rejection reads identically whichever check produces it.

- [ ] **Step 4: Run the guard before compiling**

In `plan.go`, `docEntry.planFor` has two paths that reach `compilePlan`. Give both the guard. Replace the function body:

```go
func (d *docEntry) planFor(s *Schema, e *Executor, op *ast.OperationDefinition, vars map[string]any) (*plan, bool, []*Error) {
	if len(d.condVars) > maxCondVars {
		cond := make(map[string]bool, len(d.condVars))
		for _, name := range d.condVars {
			cond[name], _ = vars[name].(bool)
		}
		return d.compile(s, e, op, cond, planKey{}, false)
	}
	variant, cond := variantKey(d.condVars, vars)
	key := planKey{op: op.Name, variant: variant}

	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.plans[key]; ok {
		return p, true, nil
	}
	return d.compile(s, e, op, cond, key, true)
}

// compile runs the pre-compile guard and then compiles. store is false for the
// uncached path taken when the document has more conditional variables than
// maxCondVars.
func (d *docEntry) compile(s *Schema, e *Executor, op *ast.OperationDefinition, cond map[string]bool, key planKey, store bool) (*plan, bool, []*Error) {
	m := operationMetrics(s, d.doc, op, cond)
	if err := e.rejectByMetrics(m); err != nil {
		return nil, false, []*Error{err}
	}
	p, errs := compilePlan(s, e, d.doc, op, cond)
	if errs != nil {
		return nil, false, errs
	}
	p.complexity = m.complexity
	p.depth = m.depth
	if store {
		if d.plans == nil {
			d.plans = make(map[planKey]*plan, 1)
		}
		d.plans[key] = p
	}
	return p, false, nil
}
```

`e` may be nil in tests that call `compilePlan` directly; `rejectByMetrics` is a method on `*Executor`, so guard the call if `planFor` can be reached with a nil executor. Check `plan_test.go` for such callers before assuming it cannot, and add `if e != nil && ...` if it can.

- [ ] **Step 5: Stop computing metrics from the plan**

In `compilePlan` (`plan.go:107`), delete the two lines that set them — `compile` now supplies both:

```go
	p.complexity = complexityOf(p.sel)
	p.depth = depthOf(p.sel)
```

- [ ] **Step 6: Move the plan-tree walks to the oracle file**

Cut `complexityOf` and `complexityMemo` from `plan.go`, and `depthOf` and `depthMemo` from `limits.go`, into a new `plan_metrics_oracle_test.go`. They are no longer production code; they are the specification `plan_metrics.go` is checked against.

```go
package graphql

// The walks below computed depth and complexity from the compiled plan until
// operationMetrics replaced them with an equivalent walk over the document,
// which can run before the plan is built. They are kept as the oracle that
// pins the replacement: TestOperationMetricsMatchesPlan and
// FuzzOperationMetrics compare the two. Do not reintroduce them into the
// production path.

func planMetricsOracle(p *plan) planMetrics {
	return planMetrics{
		complexity: complexityOf(p.sel),
		depth:      depthOf(p.sel),
	}
}
```

followed by the four functions moved verbatim.

- [ ] **Step 7: Point the equivalence test at the oracle**

In `plan_metrics_test.go`, `TestOperationMetricsMatchesPlan` currently compares against `p.complexity` and `p.depth`, which now come from `operationMetrics` itself and would compare it to itself. Compare against the oracle instead:

```go
			want := planMetricsOracle(p)
			got := operationMetrics(s, entry.doc, op, nil)
			if got.complexity != want.complexity {
				t.Errorf("complexity = %d, oracle says %d", got.complexity, want.complexity)
			}
			if got.depth != want.depth {
				t.Errorf("depth = %d, oracle says %d", got.depth, want.depth)
			}
```

This is the step that keeps the test honest. Skipping it leaves a test that cannot fail.

- [ ] **Step 8: Run the guard test and the suite**

Run: `go vet ./... && go test -race -count=1 .`
Expected: PASS, including `TestDepthLimitRejectsBeforeCompiling`.

Two failures are expected here and are not bugs in this task:
- Any test asserting that a depth- or complexity-rejected response carries `extensions.cost` will now fail. Spec §7 records this: the cost is computed from a plan the guard refuses to build. Update such a test to assert the absence, with a comment pointing at the spec.
- Any test calling `compilePlan` directly and reading `p.complexity`/`p.depth` will see zero. Point it at `planFor` or at `operationMetrics`.

- [ ] **Step 9: Verify the guard is what makes it fast**

Temporarily change `compile` to ignore the guard by replacing `if err := e.rejectByMetrics(m); err != nil` with `if err := e.rejectByMetrics(m); false`.

Run: `go test -race -count=1 -run TestDepthLimitRejectsBeforeCompiling .`
Expected: FAIL on the duration assertion. Restore, re-run, confirm PASS.

- [ ] **Step 10: Commit**

```bash
git add plan.go limits.go plan_metrics_test.go plan_metrics_oracle_test.go plan_fanout_test.go
git commit -m "fix: reject over-limit operations before compiling the plan

complexity and depth were computed from the finished plan, so
rejectIfOverLimit could not run until the expansion it would have prevented
had already happened: WithMaxDepth(3) against a depth-5 fan-out reported the
violation after 1.65s of planning. operationMetrics now computes both from the
document before compilePlan runs.

rejectIfOverLimit keeps its call site and its per-request semantics, reading
the same numbers off the cached plan. A query rejected by the guard carries no
extensions.cost, because the cost is computed from the plan the guard refuses
to build."
```

---

### Task 6: Differential fuzz, race coverage, and the full gate

**Files:**
- Modify: `plan_metrics_test.go`
- Create: `plan_fanout_race_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2-5.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Write the differential fuzz target**

Append to `plan_metrics_test.go`. The corpus seeds come from `metricsCases` so the fuzzer starts from the constructs most likely to diverge.

```go
// FuzzOperationMetrics is what keeps operationMetrics and the plan-tree oracle
// from drifting. The AST walk reproduces a walk over a structure built by
// different code; nothing but a differential check will notice when one of
// them learns about a construct and the other does not.
func FuzzOperationMetrics(f *testing.F) {
	for _, tc := range metricsCases {
		f.Add(tc.query)
	}
	s, e := newFanExecutor(&testing.T{})
	f.Fuzz(func(t *testing.T, query string) {
		entry, errs := e.document(query)
		if errs != nil {
			return // not a valid document against this schema
		}
		if len(entry.doc.Operations) == 0 {
			return
		}
		op := entry.doc.Operations[0]
		p, perrs := compilePlan(s, e, entry.doc, op, nil)
		if perrs != nil {
			return
		}
		want := planMetricsOracle(p)
		got := operationMetrics(s, entry.doc, op, nil)
		if got != want {
			t.Fatalf("operationMetrics = %+v, oracle = %+v, query = %q", got, want, query)
		}
	})
}
```

`newFanExecutor` takes `*testing.T` and calls `t.Helper()` and `t.Fatal`; passing `&testing.T{}` from a fuzz harness will panic on failure rather than report cleanly. If building the schema can fail here, extract the body of `newFanExecutor` into `buildFanExecutor() (*Schema, *Executor, error)` and have both the test helper and the fuzz target call it. Do that rather than leaving a harness that reports a schema bug as a panic.

- [ ] **Step 2: Run the fuzzer**

Run: `go test -race -count=1 -run FuzzOperationMetrics .`
Expected: PASS on the seed corpus.

Run: `go test -run xxx -fuzz FuzzOperationMetrics -fuzztime 60s .`
Expected: no crashers. If one appears, it is a genuine divergence: `plan_metrics.go` is wrong and the oracle is right. Fix `plan_metrics.go`, keep the crasher in `testdata/`, and commit both.

- [ ] **Step 3: Write the race test**

Create `plan_fanout_race_test.go`:

```go
package graphql

import (
	"sync"
	"testing"
)

// TestFanOutSharedSelectionSetsRace covers what memoization introduced: one
// selection set is now reached by many paths, and concurrent sibling
// resolvers read it at once. It is only meaningful under -race.
func TestFanOutSharedSelectionSetsRace(t *testing.T) {
	s, e := newFanExecutor(t)
	query := fanQuery(4)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.Execute(t.Context(), &Request{Query: query})
			if len(resp.Errors) != 0 {
				t.Errorf("unexpected errors: %v", resp.Errors)
			}
		}()
	}
	wg.Wait()
	_ = s
}

// TestFanOutConcurrentCompileRace drives the compile path itself rather than
// the cached plan: sixteen goroutines race to be the one that compiles.
func TestFanOutConcurrentCompileRace(t *testing.T) {
	for i := 0; i < 8; i++ {
		s, e := newFanExecutor(t)
		query := fanQuery(3)
		var wg sync.WaitGroup
		for j := 0; j < 16; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if resp := e.Execute(t.Context(), &Request{Query: query}); len(resp.Errors) != 0 {
					t.Errorf("unexpected errors: %v", resp.Errors)
				}
			}()
		}
		wg.Wait()
		_ = s
	}
}
```

- [ ] **Step 4: Run the race tests**

Run: `go test -race -count=1 -run 'TestFanOut.*Race' .`
Expected: PASS with no race reports.

- [ ] **Step 5: Run the full gate across all four modules**

Run: `sh scripts/gate.sh`
Expected: PASS. `go test ./...` reaches one of this repository's four modules; `benchmarks/`, `compare/` and `lint/` are outside the root module and only the gate script reaches them.

- [ ] **Step 6: Check the hot path did not regress**

`plan.go` and `limits.go` are on the request path. `queryCostOf` now allocates a map per call when cost is enabled.

```bash
git stash
go test -count=10 -run '^$' -bench . -benchmem > /tmp/old.txt
git stash pop
go test -count=10 -run '^$' -bench . -benchmem > /tmp/new.txt
benchstat /tmp/old.txt /tmp/new.txt
```

Expected: no significant regression on the request-path benchmarks. Single samples on this codebase have been wrong by 20-77% on a warm machine, which is why this is `-count=10` through `benchstat` and not a pair of eyeballed runs.

If `queryCostOf`'s map allocation shows up, note the number in the commit message; do not optimize it in this task.

- [ ] **Step 7: Commit**

```bash
git add plan_metrics_test.go plan_fanout_race_test.go
git commit -m "test: fuzz operationMetrics against the plan-tree oracle

The AST walk reproduces a walk over a structure built by different code.
Nothing but a differential check notices when one of them learns about a
construct and the other does not."
```

- [ ] **Step 8: Update CLAUDE.md**

The architecture section describes plan compile. Add the invariant a future reader needs, in the paragraph on `plan.go`:

> Abstract parents expand through a memo keyed on `(parent type, selection set)`, so the expansion is a DAG rather than a tree — without it an interface with N implementers selected D deep builds N^D selection sets (12 types at depth 6 measured 20.2s and 42.3M sets). The memo key is a fingerprint of AST node identities, not of the selection slice: `buildField` allocates a fresh slice when a response key has more than one AST node, so a pointer key is defeated by `{ a { b } a { b } }`. Every walk over a plan must memoize on `*selectionSet` for the same reason, `queryCostOf` included — it runs per request. Depth and complexity are computed by `operationMetrics` from the document before `compilePlan`, so the limits refuse a query instead of reporting on one already expanded; `plan_metrics_oracle_test.go` holds the walk they replaced and the two are fuzzed against each other.

```bash
git add CLAUDE.md
git commit -m "docs: record the plan expansion memo and its key invariant"
```

---

## Self-Review

**Spec coverage**

| Spec section | Task |
|---|---|
| §4.1 memo table | Task 2 steps 3-5 |
| §4.2 content-based key | Task 2 steps 3, 5; test at Task 2 step 1 (`TestFanOutMemoSurvivesDuplicateKeys`) |
| §4.3 memoize `complexityOf`/`depthOf` | Task 2 steps 6-7 |
| §4.3 memoize `queryCostOf` per request | Task 3 |
| §5.1 one implementation, moved to AST | Task 4; oracle retired in Task 5 step 6 |
| §5.2 guard runs in `planFor`, both branches | Task 5 step 4 |
| §5.2 `rejectIfOverLimit` call site unchanged | Task 5 step 3 (not modified) |
| §5.3 `QueryCost.Max` stays post-compile | Task 5 step 3 (only the two static limits move) |
| §6 fan-out regression | Task 2 step 1 |
| §6 deliberate break | Task 2 step 9, Task 3 step 5, Task 5 step 9 |
| §6 memo defeat vector | Task 2 step 1 |
| §6 guard | Task 5 step 1 |
| §6 differential fuzz | Task 6 steps 1-2 |
| §6 `-race` | Task 6 steps 3-4 |
| §6 cost memo | Task 3 step 1 |
| §7 duplicate errors pinned then changed | Task 1; Task 2 step 10 |
| §7 no change to reported numbers | Task 2 step 11, Task 3 step 6 |
| §7 guard-rejected query loses `extensions.cost` | Task 5 step 8 |

**Type consistency** — `selKey{parent any, sels string}` is defined in Task 2 and consumed unchanged in Task 4. `planMetrics{complexity, depth int}` is defined in Task 4 and consumed in Task 5. `fieldCost` gains its fourth parameter in Task 3 and no later task calls it. `newFanExecutor`, `fanQuery`, `fanPlan`, `countSelectionSets` are defined in Task 2 and used in Tasks 3, 4, 5, 6 with the same signatures.

**Known soft spots, flagged rather than hidden**

- Task 1's fixture invents a custom-scalar spelling from `scalar.go:19`. If `Writer.String` is not the marshal helper's name, the test's assertion still stands; fix the spelling against `scalar_test.go`.
- Task 5 step 4 notes that `e` may be nil on some `planFor` paths. This was not verified against `plan_test.go`; check before assuming.
- Task 6 step 1 notes that `newFanExecutor` is not fuzz-safe and says what to do about it.
