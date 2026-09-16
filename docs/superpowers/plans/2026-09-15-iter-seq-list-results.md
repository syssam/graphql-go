# iter.Seq List Results Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a composite list field return `iter.Seq[E]` or `iter.Seq[*E]` anywhere it can return `[]E` or `[]*E`, so a resolver backed by a cursor never builds a slice.

**Architecture:** No new public API. Seq types are recognised structurally and registered into the machinery that already accepts `[]E`/`[]*E`: typed traversers and nil checks in `registerObjectShapes`/`registerAbstractShapes`, element-type derivation in `shapeFor`, and one more accepted list level in `checkOutputCompositeShape`. The executor is untouched — `shape.traverse` is already push-based.

**Tech Stack:** Go 1.27, stdlib `iter` and `reflect`, `gqlparser/v2` AST. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-15-iter-seq-list-results-design.md`

## Global Constraints

- **Root package may depend only on `gqlparser/v2` and the standard library.** `iter` is stdlib; anything else here is a violation.
- **No reflection on the request hot path.** Reflection is allowed at `NewSchema`. `seqElem` runs at schema build time only — never from a traverser or writer.
- **`pushWave` must never take an estimated count.** `WaveCoordinator.ready` gates on `begun >= announced`; an inexact count either degrades DataLoader batching to N+1 or hangs parked loaders. Concurrent lists drain first. This plan must not change `pushWave` or `writeListConcurrent`.
- **The gate is `go vet ./... && go test -race -count=1 ./...`.** `-race` is not optional.
- Go 1.27 minimum. Comments explain why, not what. English only.

## File Structure

- `registry.go` — `seqElem`, seq traversers and nil checks, seq element derivation in `shapeFor`. All shape machinery already lives here.
- `validate.go` — accept a seq as one list level in `checkOutputCompositeShape`.
- `registry_test.go` — unit tests for `seqElem`.
- `object_test.go` — build-time acceptance, runtime parity, nil and early-termination tests.
- `abstract_test.go` — interface/union seq lists.
- `fixture_test.go` — one seq-returning field on the shared fixture.

**Conflict warning:** at the time of writing another effort has uncommitted changes in `fixture_test.go`, `schema.go`, `coerce.go` and `introspection.go`. Rebase before Task 3 and expect to reconcile `fixture_test.go`.

---

### Task 1: Structural seq detection

**Files:**
- Modify: `registry.go` (add `seqElem` after `reflectIsNil`, ~line 357)
- Test: `registry_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func seqElem(t reflect.Type) (reflect.Type, bool)` — the element type of an `iter.Seq`-shaped func type, and whether it matched.

- [ ] **Step 1: Write the failing test**

Add to `registry_test.go`, adding `iter` and `reflect` to its imports:

```go
func TestSeqElem(t *testing.T) {
	type post struct{ ID string }
	cases := []struct {
		name string
		typ  reflect.Type
		want reflect.Type
	}{
		{"seq of pointer", reflect.TypeFor[iter.Seq[*post]](), reflect.TypeFor[*post]()},
		{"seq of value", reflect.TypeFor[iter.Seq[post]](), reflect.TypeFor[post]()},
		{"seq of string", reflect.TypeFor[iter.Seq[string]](), reflect.TypeFor[string]()},
		{"slice is not a seq", reflect.TypeFor[[]*post](), nil},
		{"seq2 is not a seq", reflect.TypeFor[iter.Seq2[int, *post]](), nil},
		{"func returning bool", reflect.TypeFor[func() bool](), nil},
		{"yield returning nothing", reflect.TypeFor[func(func(*post))](), nil},
		{"yield returning non-bool", reflect.TypeFor[func(func(*post) error)](), nil},
		{"variadic", reflect.TypeFor[func(...func(*post) bool)](), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := seqElem(tc.typ)
			if tc.want == nil {
				if ok {
					t.Fatalf("seqElem(%s) matched %s, want no match", tc.typ, got)
				}
				return
			}
			if !ok {
				t.Fatalf("seqElem(%s) did not match, want %s", tc.typ, tc.want)
			}
			if got != tc.want {
				t.Fatalf("seqElem(%s) = %s, want %s", tc.typ, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestSeqElem .`
Expected: FAIL — `undefined: seqElem`.

- [ ] **Step 3: Write minimal implementation**

Add to `registry.go` after `reflectIsNil`:

```go
// seqElem reports the element type of an iter.Seq-shaped function type,
// func(yield func(E) bool). Matching is structural rather than on iter's
// package path: a caller's own equivalent behaves identically and there is
// no reason to reject it. Schema build time only.
func seqElem(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() != reflect.Func || t.IsVariadic() || t.NumIn() != 1 || t.NumOut() != 0 {
		return nil, false
	}
	y := t.In(0)
	if y.Kind() != reflect.Func || y.IsVariadic() || y.NumIn() != 1 || y.NumOut() != 1 {
		return nil, false
	}
	if y.Out(0).Kind() != reflect.Bool {
		return nil, false
	}
	return y.In(0), true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestSeqElem .`
Expected: PASS, all nine subtests.

- [ ] **Step 5: Commit**

```
git add registry.go registry_test.go
git commit -m "feat: detect iter.Seq shapes structurally"
```

---

### Task 2: Accept a seq as a list level at schema build

**Files:**
- Modify: `validate.go:63-72` (`checkOutputCompositeShape`)
- Modify: `registry.go:328-349` (`shapeFor`: nil fallback and element derivation)
- Test: `object_test.go`

**Interfaces:**
- Consumes: `seqElem` from Task 1.
- Produces: `NewSchema` accepts a field bound to `iter.Seq[*E]` for an SDL object list; `shapeFor` returns a `*valueShape` whose `isNil` detects a nil seq and whose `elem` is the element's typed shape.

- [ ] **Step 1: Write the failing test**

Add to `object_test.go`, adding `iter` to its imports:

```go
func TestSeqShapeAcceptedAtBuild(t *testing.T) {
	type post struct{ Title string }
	s, err := NewSchema(SDL(`type Post { title: String! } type Query { posts: [Post!]! }`),
		Object[post]("Post",
			Field("title", func(v *post) string { return v.Title }),
		),
		Query(
			Resolve("posts", func(ctx context.Context, _ Root) (iter.Seq[*post], error) {
				return nil, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema rejected an iter.Seq result: %v", err)
	}
	if s == nil {
		t.Fatal("nil schema")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestSeqShapeAcceptedAtBuild .`
Expected: FAIL — `has fewer list levels than [Post!]!`.

- [ ] **Step 3: Write minimal implementation**

In `validate.go`, replace the list-level walk at the top of `checkOutputCompositeShape`:

```go
	t := goType
	for sdlT := sdl; sdlT.Elem != nil; sdlT = sdlT.Elem {
		if e, isSeq := seqElem(t); isSeq {
			t = e
			continue
		}
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return nil, fmt.Errorf("Go type %s has fewer list levels than %s", goType, sdl.String())
		}
		t = t.Elem()
	}
	if _, isSeq := seqElem(t); isSeq {
		return nil, fmt.Errorf("Go type %s has more list levels than %s", goType, sdl.String())
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		return nil, fmt.Errorf("Go type %s has more list levels than %s", goType, sdl.String())
	}
```

In `registry.go`, add `reflect.Func` to `shapeFor`'s nil fallback. `reflectIsNil` already handles `Func`; only this dispatch omitted it, which is why a nil seq would have been called rather than written as null:

```go
	s := &valueShape{isNil: r.nilChecks[t]}
	if s.isNil == nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Interface, reflect.Map, reflect.Func:
			s.isNil = reflectIsNil
		}
	}
```

Still in `shapeFor`, derive the element shape for a seq so the element keeps its `toPtr` and nested traverser instead of degrading to an untyped shape. Replace the existing `if t.Kind() == reflect.Slice || t.Kind() == reflect.Array { ... } else { ... }`:

```go
		switch e, isSeq := seqElem(t); {
		case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
			s.elem = r.shapeFor(t.Elem(), sdl.Elem, obj)
		case isSeq:
			s.elem = r.shapeFor(e, sdl.Elem, obj)
		default:
			s.elem = &valueShape{isNil: reflectIsNil}
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -run TestSeqShapeAcceptedAtBuild .`
Expected: PASS.

Then confirm nothing regressed: `go test -race -count=1 .`
Expected: PASS. A `slog.Warn` about reflection traversal may appear; Task 3 removes it.

- [ ] **Step 5: Commit**

```
git add validate.go registry.go object_test.go
git commit -m "feat: accept iter.Seq as a list level at schema build"
```

---

### Task 3: Typed traversers for object seqs

**Files:**
- Modify: `registry.go:262-292` (`registerObjectShapes`)
- Test: `fixture_test.go`, `object_test.go`

**Interfaces:**
- Consumes: `seqElem` (Task 1), accepted shapes (Task 2).
- Produces: `registry.traversers` and `registry.nilChecks` entries for `iter.Seq[E]` and `iter.Seq[*E]`, so seq lists traverse without reflection and log no warning.

- [ ] **Step 1: Write the failing test**

In `fixture_test.go`, add to the `Query` SDL block:

```
  postsSeq: [Post!]!
```

and to the `Query(...)` bindings, adding `iter` to the file's imports:

```go
			Resolve("postsSeq", func(ctx context.Context, _ Root) (iter.Seq[*Post], error) {
				posts := f.store.posts
				return func(yield func(*Post) bool) {
					for _, p := range posts {
						if !yield(p) {
							return
						}
					}
				}, nil
			}),
```

Then add to `object_test.go`:

```go
// The seq and slice spellings of the same list must be indistinguishable in
// the response, which is the whole contract of this feature.
func TestSeqListMatchesSliceList(t *testing.T) {
	_, e := newFixtureExecutor(t)
	slice := run(t, e, `{posts{id title}}`, "")
	seq := run(t, e, `{postsSeq{id title}}`, "")
	if len(seq.Errors) != 0 {
		t.Fatalf("seq list errored: %v", seq.Errors)
	}
	want := strings.Replace(string(slice.Data), `"posts"`, `"postsSeq"`, 1)
	if got := string(seq.Data); got != want {
		t.Fatalf("seq list = %s, want %s", got, want)
	}
}

// The spec puts Field/FieldArgs in scope alongside Resolve. Shapes are
// registered per Go type, not per constructor, so a pure field must accept a
// seq too; this pins that rather than assuming it.
func TestSeqListFromPureField(t *testing.T) {
	type post struct{ Title string }
	posts := []*post{{Title: "a"}, {Title: "b"}}
	s, err := NewSchema(SDL(`type Post { title: String! } type Query { posts: [Post!]! }`),
		Object[post]("Post", Field("title", func(v *post) string { return v.Title })),
		Query(Field("posts", func(_ Root) iter.Seq[*post] {
			return func(yield func(*post) bool) {
				for _, p := range posts {
					if !yield(p) {
						return
					}
				}
			}
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema rejected a pure seq field: %v", err)
	}
	resp := run(t, NewExecutor(s), `{posts{title}}`, "")
	if got := string(resp.Data); got != `{"posts":[{"title":"a"},{"title":"b"}]}` {
		t.Fatalf("data = %s", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race -run 'TestSeqListMatchesSliceList|TestSeqListFromPureField' .`
Expected: FAIL or panic — the reflection traverser cannot index a func.

- [ ] **Step 3: Write minimal implementation**

Add `iter` to `registry.go` imports, then extend `registerObjectShapes` alongside the existing `[]E`/`[]*E` registrations:

```go
	tQE := reflect.TypeFor[iter.Seq[E]]()
	tQPE := reflect.TypeFor[iter.Seq[*E]]()
	r.nilChecks[tQE] = func(v any) bool { q, _ := v.(iter.Seq[E]); return q == nil }
	r.nilChecks[tQPE] = func(v any) bool { q, _ := v.(iter.Seq[*E]); return q == nil }
	// The executor's traverser contract carries an index for error paths and
	// a seq does not, so it is counted here.
	r.traversers[tQE] = func(v any, yield func(int, any) bool) {
		i := 0
		for e := range v.(iter.Seq[E]) {
			if !yield(i, e) {
				return
			}
			i++
		}
	}
	r.traversers[tQPE] = func(v any, yield func(int, any) bool) {
		i := 0
		for e := range v.(iter.Seq[*E]) {
			if !yield(i, e) {
				return
			}
			i++
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -run 'TestSeqListMatchesSliceList|TestSeqListFromPureField' .`
Expected: PASS, with no reflection warning logged.

- [ ] **Step 5: Commit**

```
git add registry.go fixture_test.go object_test.go
git commit -m "feat: traverse iter.Seq object lists without reflection"
```

---

### Task 4: Nil seqs and early termination

**Files:**
- Test: `object_test.go`

**Interfaces:**
- Consumes: Tasks 1-3. Adds no production code — a failure here is a defect in Task 2 (nil dispatch) or Task 3 (early return) and is fixed there.

- [ ] **Step 1: Write the failing tests**

```go
// A nil seq is null, not a call into a nil func.
func TestNilSeqWritesNull(t *testing.T) {
	type post struct{ Title string }
	s, err := NewSchema(SDL(`type Post { title: String! } type Query { posts: [Post!] }`),
		Object[post]("Post", Field("title", func(v *post) string { return v.Title })),
		Query(Resolve("posts", func(ctx context.Context, _ Root) (iter.Seq[*post], error) {
			return nil, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := run(t, NewExecutor(s), `{posts{title}}`, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("nil seq errored: %v", resp.Errors)
	}
	if got := string(resp.Data); got != `{"posts":null}` {
		t.Fatalf("data = %s, want {\"posts\":null}", got)
	}
}

// A consumer that stops must stop the producer. A seq that keeps yielding
// after a non-null element fails would do unbounded work for a dead list.
func TestSeqStopsWhenConsumerStops(t *testing.T) {
	type post struct{ Title string }
	yielded := 0
	s, err := NewSchema(SDL(`type Post { title: String! } type Query { posts: [Post!]! }`),
		Object[post]("Post", Field("title", func(v *post) string { return v.Title })),
		Query(Resolve("posts", func(ctx context.Context, _ Root) (iter.Seq[*post], error) {
			return func(yield func(*post) bool) {
				for i := 0; i < 100; i++ {
					yielded++
					var p *post // nil fails the non-null element position
					if i != 3 {
						p = &post{Title: "t"}
					}
					if !yield(p) {
						return
					}
				}
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	resp := run(t, NewExecutor(s), `{posts{title}}`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("want an error for a null element in a non-null position")
	}
	if yielded > 5 {
		t.Fatalf("producer yielded %d times after the consumer stopped", yielded)
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test -race -run 'TestNilSeqWritesNull|TestSeqStopsWhenConsumerStops' .`
Expected: both PASS if Tasks 2 and 3 are correct. A panic on a nil func call means the `reflect.Func` case from Task 2 is missing. A high yield count means the Task 3 traverser is not honouring `yield`'s false return.

- [ ] **Step 3: Fix the earlier task if either fails**

Do not special-case here. Return to Task 2 or Task 3, fix it there, and re-run both.

- [ ] **Step 4: Run the full package under race**

Run: `go test -race -count=1 .`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add object_test.go
git commit -m "test: cover nil seqs and early termination of seq lists"
```

---

### Task 5: Abstract lists, batching and the allocation claim

**Files:**
- Modify: `registry.go:295-309` (`registerAbstractShapes`)
- Modify: `fixture_test.go` (add `searchSeq`)
- Test: `abstract_test.go`, `object_test.go`, `loader/loader_test.go`
- Modify: `docs/performance.md`

**Interfaces:**
- Consumes: Tasks 1-4.
- Produces: `iter.Seq[T]` accepted for interface and union lists; a benchmark substantiating "the resolver does not allocate the slice".

- [ ] **Step 1: Write the failing tests**

The fixture binds its union as `Union[any]("SearchResult")` with `search(term: String!): [SearchResult!]!` returning `[]any`, so the seq spelling is `iter.Seq[any]`. In `fixture_test.go`, add `searchSeq(term: String!): [SearchResult!]!` to the `Query` SDL block and bind it:

```go
			ResolveArgs("searchSeq", func(_ context.Context, _ Root, a termArgs) (iter.Seq[any], error) {
				hits, err := f.store.search(a.Term)
				if err != nil {
					return nil, err
				}
				return func(yield func(any) bool) {
					for _, h := range hits {
						if !yield(h) {
							return
						}
					}
				}, nil
			}),
```

Match the existing `search` binding's body for how it obtains `hits`. Then in `abstract_test.go`:

```go
// A union list is traversed by the same machinery as an object list, so the
// seq spelling must be indistinguishable here too.
func TestSeqAbstractListMatchesSlice(t *testing.T) {
	_, e := newFixtureExecutor(t)
	slice := run(t, e, `{search(term:"a"){__typename}}`, "")
	seq := run(t, e, `{searchSeq(term:"a"){__typename}}`, "")
	if len(seq.Errors) != 0 {
		t.Fatalf("seq union list errored: %v", seq.Errors)
	}
	want := strings.Replace(string(slice.Data), `"search"`, `"searchSeq"`, 1)
	if got := string(seq.Data); got != want {
		t.Fatalf("seq union list = %s, want %s", got, want)
	}
}
```

The batching guard belongs in `loader/loader_test.go`, next to the existing
`TestLoaderBatchesConcurrentResolvers`, because that is where the loader
fixture lives. Add a seq variant of `newLoaderSchema` — identical except the
`Query` binding — and the test:

```go
func newLoaderSeqSchema(t *testing.T, ld *loader.Loader[graphql.ID, *loadOwner], items []*loadItem) *graphql.Executor {
	t.Helper()
	s, err := graphql.NewSchema(graphql.SDL(`
		type User { name: String! }
		type Item { owner: User! }
		type Query { items: [Item!]! }
	`),
		graphql.Object[loadOwner]("User",
			graphql.Field("name", func(u *loadOwner) string { return u.Name }),
		),
		graphql.Object[loadItem]("Item",
			graphql.Resolve("owner", func(ctx context.Context, it *loadItem) (*loadOwner, error) {
				return ld.Load(ctx, it.OwnerID)
			}),
		),
		graphql.Query(graphql.Resolve("items", func(context.Context, graphql.Root) (iter.Seq[*loadItem], error) {
			return func(yield func(*loadItem) bool) {
				for _, it := range items {
					if !yield(it) {
						return
					}
				}
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

// A seq list whose element has a resolver must still batch. The concurrent
// path drains the seq before announcing the wave, and that drain is what
// keeps batching intact. Two batches here means something began streaming
// into pushWave, which is the N+1 regression this engine exists to avoid.
func TestLoaderBatchesSeqList(t *testing.T) {
	var mu sync.Mutex
	var batches [][]graphql.ID
	ld := loader.New(func(_ context.Context, keys []graphql.ID) (map[graphql.ID]*loadOwner, error) {
		mu.Lock()
		batches = append(batches, append([]graphql.ID(nil), keys...))
		mu.Unlock()
		out := make(map[graphql.ID]*loadOwner, len(keys))
		for _, k := range keys {
			out[k] = &loadOwner{Name: "u" + string(k)}
		}
		return out, nil
	})

	e := newLoaderSeqSchema(t, ld, []*loadItem{{"1"}, {"2"}, {"1"}})
	resp := run(t, e, `{ items { owner { name } } }`, "")
	expectData(t, resp, `{"items":[{"owner":{"name":"u1"}},{"owner":{"name":"u2"}},{"owner":{"name":"u1"}}]}`)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1 (N+1 not coalesced): %v", len(batches), batches)
	}
	if len(batches[0]) != 2 {
		t.Fatalf("batch keys = %v, want 2 unique ids", batches[0])
	}
}
```

Add `iter` to `loader/loader_test.go` imports.

And the allocation benchmark:

```go
func BenchmarkSeqListAllocs(b *testing.B) {
	_, e := newFixtureExecutor(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := run(b, e, `{postsSeq{id title}}`, "")
		resp.Release()
	}
}
```

If `newFixtureExecutor` and `run` take `*testing.T`, widen them to `testing.TB`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestSeq|TestAbstract' .`
Expected: the abstract seq test FAILs with a build-time rejection or a reflection traversal panic.

- [ ] **Step 3: Implement abstract seq support**

In `registerAbstractShapes`, alongside the existing `[]T` registration:

```go
	tQT := reflect.TypeFor[iter.Seq[T]]()
	r.nilChecks[tQT] = func(v any) bool { q, _ := v.(iter.Seq[T]); return q == nil }
	r.traversers[tQT] = func(v any, yield func(int, any) bool) {
		i := 0
		for e := range v.(iter.Seq[T]) {
			if !yield(i, e) {
				return
			}
			i++
		}
	}
```

- [ ] **Step 4: Verify, then measure**

Run: `go test -race -count=1 ./...`
Expected: PASS.

Then substantiate the claim through `benchstat` — single samples on this machine have been wrong by 20-77%. The command below is not interleaved: `-count` runs every count of one benchmark before the next. Its B/op and allocs/op are deterministic and sound; for timings, build the binary once and alternate separate invocations:

```
go test -run '^$' -bench 'BenchmarkSeqListAllocs|BenchmarkExecuteUsers' -benchmem -count=10 .
```

Record the slice-vs-seq B/op difference in `docs/performance.md` under a short "List results" heading. **If the sequential path does not allocate measurably less, say so in the doc rather than omitting it** — that number is the feature's entire justification.

- [ ] **Step 5: Commit**

```
git add registry.go abstract_test.go object_test.go docs/performance.md
git commit -m "feat: accept iter.Seq for abstract lists and measure the saving"
```

---

## Verification

Before calling this done, from the repository root:

```
sh scripts/gate.sh
```

Expected: `all modules pass`. `go vet ./... && go test -race ./...` reaches only the root module and is not sufficient.
