# Authorization Spine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Authorization that is compiled into the plan, decided once per request, and applied by dense-index lookup at the write — covering P1, P2, P3, P5 and P7 of the design.

**Architecture:** What an operation touches (`AuthShape`) is a principal-independent property of the compiled plan, built once and cached with it. What a principal may do with it (`Decision`) is one call per operation. Applying it is an integer compare on `planField.authIdx` at the write path, so a field that declares nothing keeps its executor untouched. Streams get their own interceptor shape rather than a patched-in directive.

**Tech Stack:** Go 1.27, `gqlparser/v2` only. No new dependencies — the root package may depend on `gqlparser/v2` and the standard library alone.

**Spec:** `docs/superpowers/specs/2026-09-16-authorization-design.md`

## Global Constraints

- Root package may depend only on `github.com/vektah/gqlparser/v2` and the standard library.
- No reflection on the request hot path.
- Go 1.27 minimum.
- Tests live beside the code in `package graphql`. Reuse `fixture_test.go` (`newFixtureExecutor`, `run`, `expectData`) and `subscription_test.go` (`newSubExecutor`, `subSource`) rather than building new schemas.
- The gate is `sh scripts/gate.sh -short` (four modules). `go test ./...` reaches one of them.
- `-race` is not optional.
- Comments explain why, not what. English only. No code-narrating comments.
- Commit messages: imperative, lower-case type prefix (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`).
- The working tree may carry concurrent work from another session. **Stage files explicitly; never `git add -A`.**

### Deviation from the spec

The spec writes `Coord Coordinate`. This codebase has no `Coordinate` type — `coordinate(typeName, field)` (`schema.go:350`) returns a plain `string`. Tasks below use `Coord string`. Update the spec's §5.1 when Task 4 lands.

## File Structure

| File | Responsibility |
|---|---|
| `authz.go` (new) | `Requirement`, `AuthSite`, `SiteKind`, `AuthShape`, `Authorizer`, `Decision`, `Outcome`. The whole public authorization vocabulary, in one file so a reader can find it by name. |
| `authz_shape.go` (new) | Building an `AuthShape` during plan compile. Separate from `authz.go` because it is compiler internals, not public surface. |
| `interceptor.go` | Gains `SubscriptionInterceptor`, its func adapter and `WithSubscriptionInterceptor`, beside the three existing interceptor kinds. |
| `subscription.go` | `Subscribe` splits at the stream-opening seam so the chain can wrap it. |
| `directive.go` | `applyDirectives` rejects bound directives on subscription root fields. |
| `plan.go` | `planField` gains `authIdx int32`; `compilePlan` builds the shape. |
| `exec.go` | `Executor` gains `authorizer` and `subInterceptors`; `WithAuthorizer`. |
| `exec_object.go` | The enforcement branch at the write path. |
| `schema.go` | `RequireAuthCoverage()` and its build-time check. |

---

### Task 1: `SubscriptionInterceptor`

Closes the demonstrated defect that nothing authorizes a subscription before its source opens.

**Files:**
- Modify: `interceptor.go` (append after the field interceptor section, ~line 78)
- Modify: `exec.go:25-42` (the `Executor` struct)
- Modify: `subscription.go:193-200` (the stream-opening seam)
- Test: `subscription_test.go`

**Interfaces:**
- Consumes: `subSource.opens atomic.Int64` and `newSubExecutor(t, opts...)` from `subscription_test.go`.
- Produces: `SubscriptionInterceptor`, `SubscriptionHandler`, `SubscriptionInterceptorFunc`, `WithSubscriptionInterceptor(...)ExecutorOption`. Task 5 registers the authorizer through this chain.

- [ ] **Step 1: Write the failing test**

Append to `subscription_test.go`:

```go
// A rejected subscription must not open its source. Asserting only on the
// returned error would pass against an interceptor that runs after the
// stream is already live, which is the leak this interceptor exists to
// close; src.opens is what makes the assertion mean something.
func TestSubscriptionInterceptorRunsBeforeTheSourceOpens(t *testing.T) {
	denied := errors.New("denied")
	src, e := newSubExecutor(t, WithSubscriptionInterceptor(
		SubscriptionInterceptorFunc(func(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error) {
			if oc.Operation.SelectionSet == nil {
				t.Error("interceptor got no operation")
			}
			return nil, denied
		}),
	))

	out, err := e.Subscribe(context.Background(), &Request{Query: `subscription { messages { id } }`})
	if !errors.Is(err, denied) {
		t.Fatalf("Subscribe error = %v, want %v", err, denied)
	}
	if out != nil {
		t.Error("Subscribe returned a channel for a rejected subscription")
	}
	if n := src.opens.Load(); n != 0 {
		t.Errorf("source opened %d times for a rejected subscription, want 0", n)
	}
}

// The chain must not disturb the ordinary path: with a pass-through
// interceptor the subscription still delivers.
func TestSubscriptionInterceptorPassesThrough(t *testing.T) {
	var saw atomic.Int64
	src, e := newSubExecutor(t, WithSubscriptionInterceptor(
		SubscriptionInterceptorFunc(func(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error) {
			saw.Add(1)
			return next(ctx, oc)
		}),
	))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := e.Subscribe(ctx, &Request{Query: `subscription { messages { id } }`})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	src.messages <- &subMessage{ID: "1"}
	resp := nextResponse(t, out)
	defer resp.Release()
	if got, want := string(resp.Data), `{"messages":{"id":"1"}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
	if saw.Load() != 1 {
		t.Errorf("interceptor ran %d times, want 1", saw.Load())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'TestSubscriptionInterceptor' .`

Expected: build failure, `undefined: WithSubscriptionInterceptor`, `undefined: SubscriptionInterceptorFunc`, `undefined: SubscriptionHandler`.

- [ ] **Step 3: Add the interceptor type**

Append to `interceptor.go`:

```go
// SubscriptionHandler opens a subscription's stream of responses.
type SubscriptionHandler func(ctx context.Context, oc *OperationContext) (<-chan *Response, error)

// SubscriptionInterceptor wraps the opening of a subscription stream. It is
// the stream-shaped counterpart of OperationInterceptor, following gRPC's
// split between UnaryInterceptor and StreamInterceptor.
//
// Operation interceptors see each event of a live subscription, which is
// too late to refuse one: by then the source is open. Refusing a
// subscription is this interceptor's job.
type SubscriptionInterceptor interface {
	InterceptSubscription(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error)
}

// SubscriptionInterceptorFunc adapts a function to SubscriptionInterceptor.
type SubscriptionInterceptorFunc func(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error)

// InterceptSubscription implements SubscriptionInterceptor.
func (f SubscriptionInterceptorFunc) InterceptSubscription(ctx context.Context, oc *OperationContext, next SubscriptionHandler) (<-chan *Response, error) {
	return f(ctx, oc, next)
}

// WithSubscriptionInterceptor registers subscription interceptors. The first
// is the outermost.
func WithSubscriptionInterceptor(is ...SubscriptionInterceptor) ExecutorOption {
	return func(e *Executor) { e.subInterceptors = append(e.subInterceptors, is...) }
}
```

- [ ] **Step 4: Add the field to `Executor`**

In `exec.go`, inside `type Executor struct`, beside the other interceptor slices:

```go
	subInterceptors   []SubscriptionInterceptor
```

- [ ] **Step 5: Wrap the stream-opening seam**

In `subscription.go`, replace the block that currently reads:

```go
	stream, serr := f.def.subscribe(ctx, args)
	if serr != nil {
		return nil, e.subscribeError(ctx, Errorf("%v", serr).WithPath(Path{{Key: f.alias}}))
	}

	out := make(chan *Response)
	go e.pump(ctx, base, f, stream, out)
	return out, nil
```

with:

```go
	// Arguments are decoded above rather than inside the handler so an
	// interceptor refusing the subscription sees the same operation context a
	// successful one would.
	open := SubscriptionHandler(func(ctx context.Context, oc *OperationContext) (<-chan *Response, error) {
		stream, serr := f.def.subscribe(ctx, args)
		if serr != nil {
			return nil, e.subscribeError(ctx, Errorf("%v", serr).WithPath(Path{{Key: f.alias}}))
		}
		out := make(chan *Response)
		go e.pump(ctx, oc, f, stream, out)
		return out, nil
	})
	for i := len(e.subInterceptors) - 1; i >= 0; i-- {
		next, si := open, e.subInterceptors[i]
		open = func(ctx context.Context, oc *OperationContext) (<-chan *Response, error) {
			return si.InterceptSubscription(ctx, oc, next)
		}
	}
	return open(ctx, base)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race -run 'TestSubscription' .`
Expected: PASS, including the pre-existing subscription tests.

- [ ] **Step 7: Prove the test can fail**

Temporarily move the interceptor loop to *after* `f.def.subscribe` runs (open the stream first, then consult the chain). Run `go test -race -run TestSubscriptionInterceptorRunsBeforeTheSourceOpens .` and confirm it FAILS on `source opened 1 times`. Revert.

This is the mutation that matters: a chain that runs in the wrong order still returns the right error.

- [ ] **Step 8: Commit**

```bash
git add interceptor.go exec.go subscription.go subscription_test.go
git commit -m "feat: add SubscriptionInterceptor, the stream-shaped interception point

Operation interceptors see each event of a live subscription, which is too
late to refuse one. Nothing ran before the source opened, so a subscription
had no authorization point at all.

Follows gRPC's split between UnaryInterceptor and StreamInterceptor rather
than patching the unary concept onto a stream.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Reject bound field directives on subscription root fields

Task 1 gives streams an interception point. This stops the old one from failing silently.

**Files:**
- Modify: `directive.go:117-133` (`applyDirectives`)
- Test: `directive_test.go` (or `subscription_test.go` if no `directive_test.go` exists — check first)

**Interfaces:**
- Consumes: `schemaBuilder.errorf`, `b.ast.Subscription`, `coordinate()` from `schema.go:350`.
- Produces: nothing other tasks depend on.

- [ ] **Step 1: Write the failing test**

```go
// A directive wraps fd.anyResolve, and a subscription root field is served
// by fd.subscribe with the per-event writer substituting its executor
// entirely. A bound directive there therefore never runs. It must be a
// build error rather than a silently absent check.
func TestBoundDirectiveOnSubscriptionRootIsRejected(t *testing.T) {
	const sdl = `
directive @guard on FIELD_DEFINITION
type Query { ping: String! }
type Subscription { ticks: Int! @guard }
`
	ch := make(chan int)
	_, err := NewSchema(SDL(sdl),
		Query(Field("ping", func(Root) string { return "pong" })),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan int, error) { return ch, nil })),
		Directive("guard", func(next FieldFunc) FieldFunc { return next }),
	)
	if err == nil {
		t.Fatal("NewSchema accepted a bound directive on a subscription root field")
	}
	for _, want := range []string{"Subscription.ticks", "@guard", "SubscriptionInterceptor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q\n got: %v", want, err)
		}
	}
}

// The rejection must be about bound directives, not about directives. A
// subscription root carrying only unbound or built-in directives still builds.
func TestUnboundDirectiveOnSubscriptionRootIsAllowed(t *testing.T) {
	const sdl = `
directive @note on FIELD_DEFINITION
type Query { ping: String! }
type Subscription { ticks: Int! @note @deprecated(reason: "x") }
`
	ch := make(chan int)
	if _, err := NewSchema(SDL(sdl),
		Query(Field("ping", func(Root) string { return "pong" })),
		Subscription(Subscribe("ticks", func(context.Context) (<-chan int, error) { return ch, nil })),
	); err != nil {
		t.Fatalf("NewSchema rejected an unbound directive: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify the first fails and the second passes**

Run: `go test -race -run 'DirectiveOnSubscriptionRoot' .`
Expected: `TestBoundDirectiveOnSubscriptionRootIsRejected` FAILS with "accepted a bound directive"; `TestUnboundDirectiveOnSubscriptionRootIsAllowed` PASSES.

The second passing now is the point: it proves the first test is not going to be satisfied by rejecting every directive.

- [ ] **Step 3: Implement the check**

In `directive.go`, replace the body of `applyDirectives`'s object loop:

```go
	for _, obj := range s.objects {
		subRoot := b.ast.Subscription != nil && obj.name == b.ast.Subscription.Name
		for _, fd := range obj.fields {
			if subRoot {
				b.rejectDirectivesOnSubscriptionRoot(obj, fd)
				continue
			}
			for i := len(fd.def.Directives) - 1; i >= 0; i-- {
				b.wrapWithDirective(s, fd, fd.def.Directives[i], coordinate(obj.name, fd.name))
			}
			for i := len(obj.def.Directives) - 1; i >= 0; i-- {
				b.wrapWithDirective(s, fd, obj.def.Directives[i], obj.name)
			}
		}
	}
```

and add:

```go
// rejectDirectivesOnSubscriptionRoot fails the build for a bound directive on
// a subscription root field. Wrapping fd.anyResolve there has no effect: the
// field is served by fd.subscribe, and the per-event writer substitutes its
// executor outright. Reporting it beats a check that quietly never runs.
func (b *schemaBuilder) rejectDirectivesOnSubscriptionRoot(obj *objectType, fd *fieldDef) {
	for _, d := range append(append([]*ast.Directive(nil), fd.def.Directives...), obj.def.Directives...) {
		if b.directives[d.Name] == nil {
			continue
		}
		b.errorf("field %s: @%s is bound, but a directive on a subscription root field never runs; use a SubscriptionInterceptor", coordinate(obj.name, fd.name), d.Name)
	}
}
```

- [ ] **Step 4: Run to verify both pass**

Run: `go test -race -run 'DirectiveOnSubscriptionRoot' .`
Expected: PASS.

- [ ] **Step 5: Run the full gate**

Run: `sh scripts/gate.sh -short`
Expected: `all modules pass`.

- [ ] **Step 6: Commit**

```bash
git add directive.go directive_test.go
git commit -m "fix: reject a bound directive on a subscription root field

A directive wraps fd.anyResolve. A subscription root field is served by
fd.subscribe, and the per-event writer substitutes its executor outright,
so the directive never runs -- measured, with @auth on a subscription root
never firing. Every test stayed green, which is what made it dangerous.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `Requirement` and its SDL extraction

**Files:**
- Create: `authz.go`
- Test: `authz_test.go`

**Interfaces:**
- Produces: `Requirement`, `NewRequirement(anyOf ...[]string) Requirement`, `(Requirement).IsZero() bool`, `(Requirement).Satisfied(held map[string]bool) bool`, `(Requirement).Scopes() []string`. Task 4 stores these on `AuthSite`; Task 5 evaluates them.

- [ ] **Step 1: Write the failing test**

Create `authz_test.go`:

```go
package graphql

import (
	"slices"
	"testing"
)

func TestRequirementSatisfied(t *testing.T) {
	held := func(names ...string) map[string]bool {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	cases := []struct {
		name string
		req  Requirement
		have map[string]bool
		want bool
	}{
		{"zero requirement admits everyone", Requirement{}, nil, true},
		{"single scope held", NewRequirement([]string{"read"}), held("read"), true},
		{"single scope missing", NewRequirement([]string{"read"}), held("write"), false},
		{"any of two, second held", NewRequirement([]string{"a"}, []string{"b"}), held("b"), true},
		{"any of two, neither held", NewRequirement([]string{"a"}, []string{"b"}), held("c"), false},
		{"all within a group", NewRequirement([]string{"a", "b"}), held("a", "b"), true},
		{"all within a group, one missing", NewRequirement([]string{"a", "b"}), held("a"), false},
		{"empty group admits everyone", NewRequirement([]string{}), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.Satisfied(tc.have); got != tc.want {
				t.Errorf("Satisfied = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequirementScopesAreDeduplicatedAndSorted(t *testing.T) {
	r := NewRequirement([]string{"write", "read"}, []string{"read", "admin"})
	got := r.Scopes()
	want := []string{"admin", "read", "write"}
	if !slices.Equal(got, want) {
		t.Errorf("Scopes = %v, want %v", got, want)
	}
}

func TestZeroRequirementIsZero(t *testing.T) {
	if !(Requirement{}).IsZero() {
		t.Error("the zero Requirement reports IsZero false")
	}
	if NewRequirement([]string{"a"}).IsZero() {
		t.Error("a non-empty Requirement reports IsZero true")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestRequirement|TestZeroRequirement' .`
Expected: build failure, `undefined: Requirement`, `undefined: NewRequirement`.

- [ ] **Step 3: Implement**

Create `authz.go`:

```go
package graphql

import (
	"slices"
)

// Requirement is an OR of ANDs: satisfied when every scope in any one group
// is held. The zero Requirement is satisfied by everyone, so a field that
// declares nothing is not accidentally locked.
//
// The shape mirrors Apollo's @requiresScopes(scopes: [[String!]!]!), which is
// the vocabulary most tooling already reads.
type Requirement struct {
	anyOf [][]string
}

// NewRequirement builds a Requirement from its groups.
func NewRequirement(anyOf ...[]string) Requirement {
	return Requirement{anyOf: anyOf}
}

// IsZero reports whether the requirement admits everyone.
func (r Requirement) IsZero() bool { return len(r.anyOf) == 0 }

// Satisfied reports whether held covers any one of the groups.
func (r Requirement) Satisfied(held map[string]bool) bool {
	if r.IsZero() {
		return true
	}
	for _, group := range r.anyOf {
		ok := true
		for _, scope := range group {
			if !held[scope] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Scopes returns every scope the requirement names, sorted and deduplicated,
// so a caller can load them from a policy source in one round trip.
func (r Requirement) Scopes() []string {
	var out []string
	for _, group := range r.anyOf {
		out = append(out, group...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race -run 'TestRequirement|TestZeroRequirement' .`
Expected: PASS.

Note the `"empty group admits everyone"` case: `NewRequirement([]string{})` has one group with no scopes, and an empty AND is true. That is deliberate — it means `@requiresScopes(scopes: [[]])` is a no-op rather than a lockout.

- [ ] **Step 5: Commit**

```bash
git add authz.go authz_test.go
git commit -m "feat: add Requirement, an OR of ANDs over scopes

Mirrors Apollo's @requiresScopes(scopes: [[String!]!]!) so the vocabulary
is one tooling already reads. The zero value admits everyone, so a field
that declares nothing is not accidentally locked.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `AuthShape` built at plan compile

**Files:**
- Modify: `authz.go` (add `SiteKind`, `AuthSite`, `AuthShape`)
- Create: `authz_shape.go`
- Modify: `plan.go:70-90` (`planField` gains `authIdx`), `plan.go:36-42` (`plan` gains `shape`)
- Test: `authz_test.go`

**Interfaces:**
- Consumes: `Requirement` (Task 3); `compilePlan`, `selectionSet`, `planField` from `plan.go`.
- Produces: `AuthShape`, `(*AuthShape).Sites() []AuthSite`, `(*AuthShape).Scopes() []string`, `AuthSite{Coord string, Field *ast.FieldDefinition, Object *ast.Definition, Kind SiteKind, Requires Requirement, Grants []string}`, `SiteOutput`/`SiteObject`/`SiteFilterArg`/`SiteInputWrite`, `planField.authIdx int32`, `(*OperationContext).AuthShape() *AuthShape`. Task 5 reads all of these.

**Scope note:** this task builds sites for `SiteOutput` only. `SiteObject`, `SiteFilterArg` and `SiteInputWrite` are declared so the enum is stable, and are populated in Plan 2 — a partial enum here would force a breaking change later.

- [ ] **Step 1: Write the failing test**

Append to `authz_test.go`:

```go
// The shape is a property of the plan, not of the caller, so the same
// document yields the same sites regardless of who asks. That is what keeps
// the plan cache from multiplying by policy.
func TestAuthShapeListsOnlyDeclaringFields(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { me: User! open: String! }
type User { id: ID! salary: Int! @requiresScopes(scopes: [["pay:read"]]) }
`
	s, err := NewSchema(SDL(sdl),
		Query(
			Resolve("me", func(context.Context, Root) (*shapeUser, error) { return &shapeUser{}, nil }),
			Field("open", func(Root) string { return "ok" }),
		),
		Object[shapeUser]("User",
			Field("id", func(u *shapeUser) ID { return ID(u.ID) }),
			Field("salary", func(u *shapeUser) int { return u.Salary }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)

	var shape *AuthShape
	e2 := NewExecutor(s, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			shape = oc.AuthShape()
			return next(ctx, oc)
		})))
	_ = e
	resp := run(t, e2, `{ open me { id salary } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if shape == nil {
		t.Fatal("no shape on the operation context")
	}

	sites := shape.Sites()
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1 (only User.salary declares)", len(sites))
	}
	if sites[0].Coord != "User.salary" {
		t.Errorf("site coord = %q, want User.salary", sites[0].Coord)
	}
	if sites[0].Kind != SiteOutput {
		t.Errorf("site kind = %v, want SiteOutput", sites[0].Kind)
	}
	if !sites[0].Requires.Satisfied(map[string]bool{"pay:read": true}) {
		t.Error("site requirement not satisfied by the scope it names")
	}
	if got := shape.Scopes(); len(got) != 1 || got[0] != "pay:read" {
		t.Errorf("Scopes = %v, want [pay:read]", got)
	}
}

// A field that declares nothing must carry authIdx -1, which is what makes
// the write-path check free for it.
func TestAuthShapeLeavesUndeclaredFieldsUnindexed(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { a: String! @requiresScopes(scopes: [["x"]]) b: String! }
`
	s, err := NewSchema(SDL(sdl),
		Query(
			Field("a", func(Root) string { return "a" }),
			Field("b", func(Root) string { return "b" }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	p, _, perrs := planForTest(t, e, `{ a b }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	sel := p.sel.forType(p.root)
	byName := map[string]int32{}
	for _, f := range sel.fields {
		byName[f.name] = f.authIdx
	}
	if byName["a"] < 0 {
		t.Errorf("declaring field a has authIdx %d, want >= 0", byName["a"])
	}
	if byName["b"] != -1 {
		t.Errorf("undeclared field b has authIdx %d, want -1", byName["b"])
	}
}

type shapeUser struct {
	ID     string
	Salary int
}
```

Add this helper to `authz_test.go` (the plan cache is internal, so the test reaches it the way `plan_test.go` does — check `plan_test.go` for an existing helper first and reuse it rather than duplicating):

```go
func planForTest(t *testing.T, e *Executor, query string) (*plan, bool, []*Error) {
	t.Helper()
	entry, errs := e.document(query)
	if errs != nil {
		t.Fatalf("document: %v", errs)
	}
	op, oerr := selectOperation(entry.doc, "")
	if oerr != nil {
		t.Fatalf("selectOperation: %v", oerr)
	}
	return entry.planFor(e.schema, e, op, nil)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestAuthShape' .`
Expected: build failure — `undefined: AuthShape`, `undefined: SiteOutput`, `f.authIdx undefined`.

- [ ] **Step 3: Add the shape types**

Append to `authz.go`:

```go
// SiteKind says what kind of position needs an authorization decision.
type SiteKind uint8

const (
	// SiteOutput is a selected output field.
	SiteOutput SiteKind = iota
	// SiteObject is a whole object type, checked once per instance rather
	// than once per field of it.
	SiteObject
	// SiteFilterArg is a coordinate named in a where, orderBy or groupBy
	// argument. A restricted field must not be filterable either, or its
	// value leaks by bisection without ever being selected.
	SiteFilterArg
	// SiteInputWrite is a coordinate written by a mutation input.
	SiteInputWrite
)

// AuthSite is one position in a plan that may need a decision.
type AuthSite struct {
	Coord    string
	Field    *ast.FieldDefinition // nil when Kind is SiteObject
	Object   *ast.Definition
	Kind     SiteKind
	Requires Requirement
	Grants   []string
}

// AuthShape is what an operation touches, independent of who is asking. It
// is computed once per compiled plan and cached with it, which is why the
// plan cache is not multiplied by the number of distinct policies.
type AuthShape struct {
	sites  []AuthSite
	scopes []string
}

// Sites returns the positions needing a decision, indexed by site index.
// planField.authIdx indexes into this slice.
func (s *AuthShape) Sites() []AuthSite {
	if s == nil {
		return nil
	}
	return s.sites
}

// Scopes returns every scope named anywhere in the operation, sorted and
// deduplicated, so an Authorizer can load them in one round trip.
func (s *AuthShape) Scopes() []string {
	if s == nil {
		return nil
	}
	return s.scopes
}

// IsEmpty reports whether the operation touches nothing that declares a
// requirement. An Authorizer is not consulted for such an operation.
func (s *AuthShape) IsEmpty() bool { return s == nil || len(s.sites) == 0 }
```

Add `"github.com/vektah/gqlparser/v2/ast"` to `authz.go`'s imports.

- [ ] **Step 4: Add the builder**

Create `authz_shape.go`:

```go
package graphql

import (
	"slices"

	"github.com/vektah/gqlparser/v2/ast"
)

// authDirective is the SDL directive read into a Requirement. It is a
// package-level name rather than an option because the shape must be
// identical for every executor sharing a schema: the shape travels with the
// cached plan, and a per-executor reading would make one executor's plan
// wrong for another's.
const authDirective = "requiresScopes"

// buildAuthShape walks a compiled selection and records every position that
// declares a requirement. It runs once per plan, so the walk is O(plan size)
// and never repeated per request.
func buildAuthShape(sel *selectionSet) *AuthShape {
	b := &shapeBuilder{}
	b.walk(sel)
	if len(b.sites) == 0 {
		return nil
	}
	slices.Sort(b.scopes)
	return &AuthShape{sites: b.sites, scopes: slices.Compact(b.scopes)}
}

type shapeBuilder struct {
	sites  []AuthSite
	scopes []string
	seen   map[*selectionSet]bool
}

func (b *shapeBuilder) walk(sel *selectionSet) {
	if sel == nil {
		return
	}
	// A plan for a recursive selection can reach the same set twice; without
	// this the walk would index the same field more than once.
	if b.seen == nil {
		b.seen = make(map[*selectionSet]bool)
	}
	if b.seen[sel] {
		return
	}
	b.seen[sel] = true

	for _, f := range sel.fields {
		b.field(f)
	}
	for _, concrete := range sel.byType {
		b.walk(concrete)
	}
}

func (b *shapeBuilder) field(f *planField) {
	f.authIdx = -1
	if f.def != nil && f.def.def != nil {
		if req, ok := requirementOf(f.def.def.Directives); ok {
			f.authIdx = int32(len(b.sites))
			b.sites = append(b.sites, AuthSite{
				Coord:    coordinate(f.def.object.name, f.name),
				Field:    f.def.def,
				Object:   f.def.object.def,
				Kind:     SiteOutput,
				Requires: req,
			})
			b.scopes = append(b.scopes, req.Scopes()...)
		}
	}
	b.walk(f.sub)
}

// requirementOf reads @requiresScopes off a definition. The argument is a
// list of lists of strings; anything else is a schema error already caught
// at NewSchema, so a malformed value here is treated as no requirement
// rather than as a lockout nobody can diagnose at request time.
func requirementOf(ds ast.DirectiveList) (Requirement, bool) {
	d := ds.ForName(authDirective)
	if d == nil {
		return Requirement{}, false
	}
	arg := d.Arguments.ForName("scopes")
	if arg == nil || arg.Value == nil {
		return Requirement{}, false
	}
	var groups [][]string
	for _, outer := range arg.Value.Children {
		var group []string
		for _, inner := range outer.Value.Children {
			group = append(group, inner.Value.Raw)
		}
		groups = append(groups, group)
	}
	if groups == nil {
		return Requirement{}, false
	}
	return Requirement{anyOf: groups}, true
}
```

If `fieldDef` has no `object` or `def` field with those names, read `object.go` and adjust — the site needs the parent `*objectType` and the `*ast.FieldDefinition`.

- [ ] **Step 5: Wire it into the plan**

In `plan.go`, add to `planField`:

```go
	// authIdx indexes this field's site in the plan's AuthShape, or -1 when
	// the field declares no requirement. An integer compare on a field
	// already in cache is what keeps authorization free for fields that
	// declare nothing.
	authIdx int32
```

add to `plan`:

```go
	shape *AuthShape
```

and at the end of `compilePlan`, after `sel` is built and before the plan is returned:

```go
	p.shape = buildAuthShape(p.sel)
```

- [ ] **Step 6: Expose the shape on the operation context**

In `context.go`, beside `Complexity()` and `Depth()`:

```go
// AuthShape returns what this operation touches, or nil when it touches
// nothing that declares an authorization requirement.
func (oc *OperationContext) AuthShape() *AuthShape {
	if oc.plan == nil {
		return nil
	}
	return oc.plan.shape
}
```

- [ ] **Step 7: Run to verify it passes**

Run: `go test -race -run 'TestAuthShape' .`
Expected: PASS.

- [ ] **Step 8: Prove the test can fail**

Change `buildAuthShape` to return `nil` unconditionally. Run `go test -race -run TestAuthShape .` and confirm both tests FAIL. Revert.

- [ ] **Step 9: Run the full gate**

Run: `sh scripts/gate.sh -short`
Expected: `all modules pass`.

- [ ] **Step 10: Update the spec's deviation and commit**

Edit `docs/superpowers/specs/2026-09-16-authorization-design.md` §5.1 to spell `Coord string`, and delete the corresponding note from this plan's Global Constraints.

```bash
git add authz.go authz_shape.go plan.go context.go authz_test.go docs/superpowers/specs/2026-09-16-authorization-design.md
git commit -m "feat: build an AuthShape at plan compile

What an operation touches is a property of the plan, not of the caller, so
it is computed once and cached with the plan. The plan cache therefore does
not multiply by the number of distinct policies, and planKey is unchanged.

Fields that declare nothing carry authIdx -1, which is what will keep the
write-path check free for them.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `Authorizer` and `Decision`

**Files:**
- Modify: `authz.go`
- Modify: `exec.go` (`Executor` gains `authorizer`; `WithAuthorizer`; the decision runs in `runOperation`)
- Test: `authz_test.go`

**Interfaces:**
- Consumes: `AuthShape`, `AuthSite` (Task 4).
- Produces: `Authorizer`, `AuthorizerFunc`, `Decision`, `(*Decision).Set(site int, o Outcome) error`, `(*Decision).Outcome(site int) Outcome`, `WithAuthorizer(a Authorizer) ExecutorOption`, `ScopeAuthorizer(held func(context.Context) map[string]bool) Authorizer`. Task 6 reads `Decision.Outcome`.

- [ ] **Step 1: Write the failing test**

```go
func TestAuthorizerRunsOncePerOperation(t *testing.T) {
	var calls atomic.Int64
	s := shapeSchema(t) // helper added in Step 3 below
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			calls.Add(1)
			return nil
		})))
	// Two User values, each selecting the declaring field: a per-field or
	// per-row authorizer would run more than once.
	resp := run(t, e, `{ a: me { salary } b: me { salary } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("authorizer ran %d times, want 1", got)
	}
}

func TestAuthorizerErrorRejectsTheOperation(t *testing.T) {
	s := shapeSchema(t)
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return Errorf("nope").WithCode("FORBIDDEN")
		})))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("operation was not rejected")
	}
	if resp.Data != nil && string(resp.Data) != "null" {
		t.Errorf("rejected operation returned data: %s", resp.Data)
	}
}

// An operation touching nothing that declares must not pay for an authorizer
// call at all.
func TestAuthorizerSkippedForAnEmptyShape(t *testing.T) {
	var calls atomic.Int64
	s := shapeSchema(t)
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			calls.Add(1)
			return nil
		})))
	run(t, e, `{ open }`, "")
	if got := calls.Load(); got != 0 {
		t.Errorf("authorizer ran %d times for an operation with no sites, want 0", got)
	}
}

func TestScopeAuthorizerDeniesUnheldScopes(t *testing.T) {
	s := shapeSchema(t)
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return map[string]bool{"other": true} })))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("unheld scope was not denied")
	}
	if !strings.Contains(resp.Errors[0].Message, "or it might not exist") {
		t.Errorf("denial does not use the AIP-211 wording: %s", resp.Errors[0].Message)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestAuthorizer|TestScopeAuthorizer' .`
Expected: build failure — `undefined: WithAuthorizer`, `undefined: AuthorizerFunc`, `undefined: ScopeAuthorizer`, `undefined: shapeSchema`.

- [ ] **Step 3: Add the test helper**

In `authz_test.go`, replace the inline schema of Task 4's first test with a shared helper and use it in both:

```go
const shapeSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { me: User! open: String! }
type User { id: ID! salary: Int! @requiresScopes(scopes: [["pay:read"]]) }
`

func shapeSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(shapeSDL),
		Query(
			Resolve("me", func(context.Context, Root) (*shapeUser, error) {
				return &shapeUser{ID: "1", Salary: 100}, nil
			}),
			Field("open", func(Root) string { return "ok" }),
		),
		Object[shapeUser]("User",
			Field("id", func(u *shapeUser) ID { return ID(u.ID) }),
			Field("salary", func(u *shapeUser) int { return u.Salary }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}
```

- [ ] **Step 4: Implement `Authorizer` and `Decision`**

Append to `authz.go`:

```go
// Authorizer turns an operation's shape into a decision for one principal.
// It runs once per operation, before any field resolves, and is not called
// at all when the operation touches nothing that declares a requirement.
//
// A returned error rejects the whole operation. The interface deliberately
// holds no policy of its own: it is the adapter to whatever decides, be that
// OPA, Cedar, OpenFGA, Casbin or a hand-written checker.
type Authorizer interface {
	Authorize(ctx context.Context, shape *AuthShape, d *Decision) error
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(ctx context.Context, shape *AuthShape, d *Decision) error

// Authorize implements Authorizer.
func (f AuthorizerFunc) Authorize(ctx context.Context, shape *AuthShape, d *Decision) error {
	return f(ctx, shape, d)
}

// WithAuthorizer sets the authorizer consulted once per operation.
func WithAuthorizer(a Authorizer) ExecutorOption {
	return func(e *Executor) { e.authorizer = a }
}

// Decision records the outcome for each site in a shape. It is passed to the
// Authorizer rather than returned by it so the framework owns the allocation
// and sizes it from the shape. The zero value of every entry allows.
type Decision struct {
	shape    *AuthShape
	outcomes []Outcome
}

func newDecision(shape *AuthShape) *Decision {
	return &Decision{shape: shape, outcomes: make([]Outcome, len(shape.Sites()))}
}

// Set records the outcome for one site. It reports an error for an outcome
// the site cannot represent, so a policy mistake surfaces once per request
// with the coordinate attached rather than as a null-bubbled parent.
func (d *Decision) Set(site int, o Outcome) error {
	if d == nil || site < 0 || site >= len(d.outcomes) {
		return Errorf("authorization: site %d is out of range", site)
	}
	if err := o.validFor(d.shape.sites[site]); err != nil {
		return err
	}
	d.outcomes[site] = o
	return nil
}

// Outcome returns the recorded outcome for a site.
func (d *Decision) Outcome(site int) Outcome {
	if d == nil || site < 0 || site >= len(d.outcomes) {
		return Outcome{}
	}
	return d.outcomes[site]
}

// ScopeAuthorizer is the default policy: a site is allowed when held covers
// its Requirement, and denied with the AIP-211 wording otherwise. It exists
// so the common case needs no Authorizer of its own.
func ScopeAuthorizer(held func(context.Context) map[string]bool) Authorizer {
	return AuthorizerFunc(func(ctx context.Context, shape *AuthShape, d *Decision) error {
		have := held(ctx)
		for i, site := range shape.Sites() {
			if site.Requires.Satisfied(have) {
				continue
			}
			scopes := site.Requires.Scopes()
			if err := d.Set(i, Deny(strings.Join(scopes, " or "), site.Coord)); err != nil {
				return err
			}
		}
		return nil
	})
}
```

Add `"context"` and `"strings"` to `authz.go`'s imports.

- [ ] **Step 5: Add the field and run the decision**

In `exec.go`, add to `Executor`:

```go
	authorizer Authorizer
```

In `runOperation`, before execution begins, build the decision:

```go
	if e.authorizer != nil && !oc.plan.shape.IsEmpty() {
		d := newDecision(oc.plan.shape)
		if err := e.authorizer.Authorize(ctx, oc.plan.shape, d); err != nil {
			return e.requestError(ctx, toError(err))
		}
		oc.decision = d
	}
```

Add `decision *Decision` to `OperationContext` **in the unexported block**, and read `exec.go` for the exact spelling of the error helper (`toError` may be named differently — grep for how `runOperation` builds an `*Error` from an `error`).

- [ ] **Step 6: Run to verify it passes**

Run: `go test -race -run 'TestAuthorizer|TestScopeAuthorizer|TestAuthShape' .`
Expected: `TestAuthorizerRunsOncePerOperation`, `TestAuthorizerErrorRejectsTheOperation` and `TestAuthorizerSkippedForAnEmptyShape` PASS. `TestScopeAuthorizerDeniesUnheldScopes` still FAILS — `Deny` and `Outcome` arrive in Task 6.

- [ ] **Step 7: Commit**

```bash
git add authz.go exec.go context.go authz_test.go
git commit -m "feat: decide authorization once per operation

The Authorizer sees the operation's shape and fills a Decision sized from
it. It runs once, before any field resolves, and not at all when the
operation touches nothing that declares a requirement.

The interface holds no policy of its own. graphql-js is right that
authorization belongs to the business layer; what it cannot account for is
that a business layer never learns which fields were selected.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `Outcome` and enforcement at the write path

**Files:**
- Modify: `authz.go` (`Outcome` and its constructors)
- Modify: `exec_object.go` (the enforcement branch)
- Test: `authz_test.go`

**Interfaces:**
- Consumes: `Decision.Outcome` (Task 5), `planField.authIdx` (Task 4).
- Produces: `Outcome`, `Allow()`, `Deny(permission, resource string)`, `Null()`, `Zero()`, `Redact(fn func(any) any)`, `Drop()`, `(Outcome).validFor(AuthSite) error`.

**Scope note:** `Drop()` needs list-traversal support and lands in Plan 2. This task declares it and has `validFor` reject it, so the constructor's signature is settled without shipping a half-working behaviour.

- [ ] **Step 1: Write the failing test**

```go
func TestOutcomeEnforcement(t *testing.T) {
	cases := []struct {
		name    string
		outcome Outcome
		query   string
		want    string
		wantErr string
	}{
		{"allow is a pass-through", Allow(), `{ me { salary } }`, `{"me":{"salary":100}}`, ""},
		{"zero replaces a non-null leaf", Zero(), `{ me { salary } }`, `{"me":{"salary":0}}`, ""},
		{"redact rewrites the value", Redact(func(any) any { return 7 }), `{ me { salary } }`, `{"me":{"salary":7}}`, ""},
		{"deny reports and bubbles", Deny("pay:read", "User.salary"), `{ me { salary } }`, "", "or it might not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := shapeSchema(t)
			e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					return d.Set(0, tc.outcome)
				})))
			resp := run(t, e, tc.query, "")
			if tc.wantErr != "" {
				if len(resp.Errors) == 0 {
					t.Fatalf("no error; data = %s", resp.Data)
				}
				if !strings.Contains(resp.Errors[0].Message, tc.wantErr) {
					t.Errorf("error = %s, want it to contain %q", resp.Errors[0].Message, tc.wantErr)
				}
				return
			}
			if len(resp.Errors) > 0 {
				t.Fatalf("errors: %s", errorsJSON(resp.Errors))
			}
			if got := string(resp.Data); got != tc.want {
				t.Errorf("data = %s, want %s", got, tc.want)
			}
		})
	}
}

// Enforcement must not depend on how a field is bound. Defect B existed
// because a pure Field and a Resolve of the same coordinate took different
// paths; this pins that they no longer do.
func TestOutcomeAppliesToPureAndResolverBindingsAlike(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { me: User! }
type User {
  pure: Int! @requiresScopes(scopes: [["x"]])
  viaResolver: Int! @requiresScopes(scopes: [["x"]])
}
`
	s, err := NewSchema(SDL(sdl),
		Query(Resolve("me", func(context.Context, Root) (*shapeUser, error) { return &shapeUser{Salary: 5}, nil })),
		Object[shapeUser]("User",
			Field("pure", func(u *shapeUser) int { return u.Salary }),
			Resolve("viaResolver", func(_ context.Context, u *shapeUser) (int, error) { return u.Salary, nil }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			for i := range shape.Sites() {
				if err := d.Set(i, Zero()); err != nil {
					return err
				}
			}
			return nil
		})))
	resp := run(t, e, `{ me { pure viaResolver } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"me":{"pure":0,"viaResolver":0}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestOutcome' .`
Expected: build failure — `undefined: Allow`, `undefined: Zero`, `undefined: Redact`, `undefined: Deny`.

- [ ] **Step 3: Implement `Outcome`**

Append to `authz.go`:

```go
type action uint8

const (
	actionAllow action = iota
	actionDeny
	actionNull
	actionZero
	actionRedact
	actionDrop
)

// Outcome is what the executor does with a site. The zero Outcome allows, so
// a Decision left untouched by an Authorizer changes nothing.
type Outcome struct {
	act        action
	redact     func(any) any
	permission string
	resource   string
}

// Allow resolves the field normally.
func Allow() Outcome { return Outcome{} }

// Deny refuses the field. The message follows AIP-211: it names neither the
// value nor whether the resource exists, because choosing between
// PERMISSION_DENIED and NOT_FOUND is itself an existence oracle.
func Deny(permission, resource string) Outcome {
	return Outcome{act: actionDeny, permission: permission, resource: resource}
}

// Null writes null without resolving the field.
func Null() Outcome { return Outcome{act: actionNull} }

// Zero writes the zero value of the field's type without resolving it. It is
// how a non-null leaf is withheld without null-bubbling its parent.
func Zero() Outcome { return Outcome{act: actionZero} }

// Redact resolves the field and rewrites the result.
func Redact(fn func(any) any) Outcome { return Outcome{act: actionRedact, redact: fn} }

// Drop omits the value from its enclosing list.
func Drop() Outcome { return Outcome{act: actionDrop} }

func (o Outcome) err(coord string) *Error {
	return Errorf("Permission %q denied on resource %q (or it might not exist).", o.permission, o.resource).
		WithCode(CodeForbidden)
}

// validFor rejects an outcome the site cannot represent. A non-null
// composite has no meaningful zero, so Zero there would null-bubble the
// parent -- which in practice turns a withheld total into a blank page. The
// site knows the field's type, so this is caught once per request with the
// coordinate attached.
func (o Outcome) validFor(site AuthSite) error {
	switch o.act {
	case actionZero:
		if site.Field == nil {
			return Errorf("authorization: Zero is not valid for %s, which is an object site", site.Coord)
		}
		if !isLeafType(site.Field.Type) {
			return Errorf("authorization: Zero is not valid for %s: %s has no zero value; use Deny or gate the parent", site.Coord, site.Field.Type.String())
		}
	case actionDrop:
		return Errorf("authorization: Drop is not yet implemented")
	}
	return nil
}
```

`CodeForbidden` may not exist — check `errors.go` for the existing code constants and either reuse the closest (`CodeUnauthorized`?) or add `CodeForbidden` beside them, following the file's existing pattern. `isLeafType` likewise: check whether the plan already has a helper for "scalar or enum after unwrapping"; reuse it rather than writing a second one.

- [ ] **Step 4: Enforce at the write path**

In `exec_object.go`, inside the per-field loop of `writeObject`, before the field is executed:

```go
		// -1 on a field that declares nothing, so this is an integer compare
		// on a struct already in cache. Growing it beyond that would put the
		// cost on every field of every request.
		if st.decision != nil && f.authIdx >= 0 {
			if handled := st.enforce(ctx, w, f, path); handled {
				continue
			}
		}
```

and add `decision *Decision` to `execState`, set from `oc.decision` where `execState` is built.

Write `enforce` in `authz_shape.go` (it is executor internals, not public surface). It must:
- read `st.decision.Outcome(int(f.authIdx))`
- `actionAllow`: return false, letting the normal path run
- `actionNull`: write the response key and a null, honouring non-null bubbling via the existing `writeNullValue`
- `actionZero`: write the response key and the zero value for `f.def.typ`
- `actionRedact`: run the field, then rewrite — this one returns false and instead wraps, so route it through the existing `fieldExec` rather than duplicating the write
- `actionDeny`: record `o.err(site.Coord)` at `path` via `st.fieldError` and write null

- [ ] **Step 5: Run to verify it passes**

Run: `go test -race -run 'TestOutcome|TestScopeAuthorizer' .`
Expected: PASS, including `TestScopeAuthorizerDeniesUnheldScopes` from Task 5.

- [ ] **Step 6: Prove the tests can fail**

Make `Decision.Set` a no-op (`return nil` without storing). Run `go test -race -run 'TestOutcome|TestScopeAuthorizer' .` and confirm every enforcement case FAILS. Revert.

- [ ] **Step 7: Run the full gate**

Run: `sh scripts/gate.sh -short`
Expected: `all modules pass`.

- [ ] **Step 8: Commit**

```bash
git add authz.go authz_shape.go exec_object.go authz_test.go errors.go
git commit -m "feat: apply authorization outcomes at the write path

A field that declares nothing carries authIdx -1, so enforcement costs it
an integer compare on a struct already in cache.

Zero on a non-null composite is refused when the Decision is built rather
than discovered as a null-bubbled parent at write time: an object has no
meaningful zero, and withholding one blanks the whole record.

Denial follows AIP-211 -- one outcome, wording that names neither the value
nor whether the resource exists, because choosing between PERMISSION_DENIED
and NOT_FOUND is itself an existence oracle.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: `RequireAuthCoverage()`

**Files:**
- Modify: `schema.go` (the option and the check in `build()`'s coverage phase)
- Test: `authz_test.go`

**Interfaces:**
- Consumes: `requirementOf` (Task 4).
- Produces: `RequireAuthCoverage() SchemaOption`.

- [ ] **Step 1: Write the failing test**

```go
// 825 SDL files cannot be held by review. A type arriving with no
// declaration is how an aggregate surface came to return hidden columns in
// full, so it must fail the build.
func TestRequireAuthCoverageRejectsAnUndeclaredField(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
directive @public on FIELD_DEFINITION | OBJECT
type Query { guarded: String! @requiresScopes(scopes: [["x"]]) forgotten: String! }
`
	_, err := NewSchema(SDL(sdl),
		Query(
			Field("guarded", func(Root) string { return "" }),
			Field("forgotten", func(Root) string { return "" }),
		),
		RequireAuthCoverage(),
	)
	if err == nil {
		t.Fatal("NewSchema accepted a field with no authorization declaration")
	}
	if !strings.Contains(err.Error(), "Query.forgotten") {
		t.Errorf("error does not name the undeclared field: %v", err)
	}
	if strings.Contains(err.Error(), "Query.guarded") {
		t.Errorf("error names a field that is declared: %v", err)
	}
}

func TestRequireAuthCoverageAcceptsPublic(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
directive @public on FIELD_DEFINITION | OBJECT
type Query { open: String! @public }
`
	if _, err := NewSchema(SDL(sdl),
		Query(Field("open", func(Root) string { return "" })),
		RequireAuthCoverage(),
	); err != nil {
		t.Fatalf("NewSchema rejected an explicitly public field: %v", err)
	}
}

// Without the option nothing changes, or every existing schema breaks.
func TestAuthCoverageIsOptIn(t *testing.T) {
	const sdl = `type Query { forgotten: String! }`
	if _, err := NewSchema(SDL(sdl),
		Query(Field("forgotten", func(Root) string { return "" })),
	); err != nil {
		t.Fatalf("NewSchema rejected an undeclared field without RequireAuthCoverage: %v", err)
	}
}

// graphql-hive/envelop#892: composing __schema into an ordinary operation
// bypassed field permissions there, because the check was skipped for
// documents it classified as introspection. This engine's rule is per-field
// (introspection.go, observers.OnField) with no whole-document shortcut, so
// it is immune by construction -- which is exactly why it needs a pin.
// Immunity by construction is one refactor away from immunity by luck.
func TestComposedIntrospectionDoesNotBypassChecks(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
directive @public on FIELD_DEFINITION | OBJECT
type Query { secret: String! @requiresScopes(scopes: [["x"]]) }
`
	s, err := NewSchema(SDL(sdl),
		Query(Field("secret", func(Root) string { return "classified" })),
		RequireAuthCoverage(),
		DisableIntrospection(),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return nil })))

	resp := run(t, e, `{ __schema { __typename } secret }`, "")
	if len(resp.Errors) == 0 {
		t.Fatalf("composed introspection was accepted; data = %s", resp.Data)
	}
	if strings.Contains(string(resp.Data), "classified") {
		t.Errorf("the guarded value was returned: %s", resp.Data)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'AuthCoverage|ComposedIntrospection' .`
Expected: the tests using `RequireAuthCoverage` fail to build (`undefined: RequireAuthCoverage`); `TestAuthCoverageIsOptIn` PASSES.

- [ ] **Step 3: Implement**

In `schema.go`, beside `DisableIntrospection`:

```go
// RequireAuthCoverage fails NewSchema for any field that declares neither an
// authorization requirement nor @public. It is opt-in because it breaks every
// schema that has not adopted it.
//
// The failure it prevents is not a field someone forgot to guard but a type
// nobody noticed arriving: a generated aggregate surface keyed by a resource
// no role grants and no role restricts is unguarded in both directions.
func RequireAuthCoverage() SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) { b.authCoverage = true })
}
```

Add `authCoverage bool` to `schemaBuilder`, and in the coverage validation phase of `build()`:

```go
func (b *schemaBuilder) validateAuthCoverage(s *Schema) {
	if !b.authCoverage {
		return
	}
	for _, obj := range s.objects {
		if isIntrospectionType(obj.name) {
			continue
		}
		if obj.def.Directives.ForName("public") != nil {
			continue
		}
		if _, ok := requirementOf(obj.def.Directives); ok {
			continue
		}
		for _, fd := range obj.fields {
			if fd.def.Directives.ForName("public") != nil {
				continue
			}
			if _, ok := requirementOf(fd.def.Directives); ok {
				continue
			}
			b.errorf("field %s declares no authorization; add @requiresScopes or @public", coordinate(obj.name, fd.name))
		}
	}
}
```

`isIntrospectionType` may not exist — check `introspection.go`; the meta types are named with a `__` prefix, so `strings.HasPrefix(obj.name, "__")` is the fallback. Call `validateAuthCoverage` from `build()` beside the existing coverage validation.

- [ ] **Step 4: Run to verify all four pass**

Run: `go test -race -run 'AuthCoverage|ComposedIntrospection' .`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

Make `validateAuthCoverage` return immediately. Confirm `TestRequireAuthCoverageRejectsAnUndeclaredField` FAILS. Revert.

- [ ] **Step 6: Commit**

```bash
git add schema.go authz_test.go
git commit -m "feat: add RequireAuthCoverage, a build-time default deny

Opt-in, because it breaks every schema that has not adopted it. The failure
it prevents is not a field someone forgot to guard but a type nobody
noticed arriving: a generated aggregate surface keyed by a resource no role
grants and no role restricts is unguarded in both directions, and review
does not catch that across 825 schema files.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: The performance gate

The spec commits to measuring rather than assuming. This task is where that happens; it may send Tasks 4 and 6 back for rework.

**Files:**
- Test: `authz_bench_test.go` (new)

- [ ] **Step 1: Record the struct sizes**

Add to `authz_bench_test.go`:

```go
// execState and OperationContext sit on size-class boundaries; CLAUDE.md
// records an atomic.Int64 on execState costing +3.2% B/op with its feature
// disabled. These are not assertions about a good number, they are a record
// of the number, so a later change that crosses a boundary is visible.
func TestStructSizes(t *testing.T) {
	t.Logf("execState        = %d bytes", unsafe.Sizeof(execState{}))
	t.Logf("OperationContext = %d bytes", unsafe.Sizeof(OperationContext{}))
	t.Logf("planField        = %d bytes", unsafe.Sizeof(planField{}))
}
```

Run: `go test -run TestStructSizes -v .` and record the three numbers in the commit message.

- [ ] **Step 2: Benchmark with authorization disabled**

```go
func BenchmarkExecuteNoAuthorizer(b *testing.B) {
	s := shapeSchemaB(b)
	e := NewExecutor(s)
	benchRun(b, e, `{ me { id salary } open }`)
}

func BenchmarkExecuteWithAuthorizer(b *testing.B) {
	s := shapeSchemaB(b)
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return map[string]bool{"pay:read": true} })))
	benchRun(b, e, `{ me { id salary } open }`)
}

func benchRun(b *testing.B, e *Executor, q string) {
	b.ReportAllocs()
	ctx := context.Background()
	req := &Request{Query: q}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Execute(ctx, req).Release()
	}
}
```

`shapeSchemaB` is `shapeSchema` taking `testing.TB`; change `shapeSchema`'s parameter to `testing.TB` and call it from both.

- [ ] **Step 3: Compare against the pre-change baseline**

```bash
git stash
go test -count=10 -run '^$' -bench 'BenchmarkExecute' -benchmem . > /tmp/old.txt
git stash pop
go test -count=10 -run '^$' -bench 'BenchmarkExecuteNoAuthorizer' -benchmem . > /tmp/new.txt
benchstat /tmp/old.txt /tmp/new.txt
```

**Acceptance:** `BenchmarkExecuteNoAuthorizer` shows no statistically significant regression against the same query before the change. CLAUDE.md records that single samples on this machine have been wrong by 20-77%; `-count=10` and `benchstat` are the gate, not eyeballing.

**If it regresses:** the fallback in the spec is to pack `decision` into existing padding in `execState`, or to reach it through `oc` rather than storing a second pointer. Do that and re-measure before proceeding.

- [ ] **Step 4: Commit**

```bash
git add authz_bench_test.go authz_test.go
git commit -m "test: pin the cost of authorization when it is disabled

Records execState, OperationContext and planField sizes so a later change
that crosses a size class is visible, and benchmarks an executor with no
authorizer against the same query before the change.

CLAUDE.md records single samples on this machine being wrong by 20-77%, so
the gate is benchstat over -count=10, not a reading.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Documentation

**Files:**
- Modify: `doc.go`
- Modify: `CLAUDE.md` (the Architecture section)

- [ ] **Step 1: Document the package surface**

Add an authorization section to `doc.go` covering: the shape/decision/enforcement split, that `Authorizer` runs once per operation, that a field declaring nothing costs nothing, that subscriptions authorize through `SubscriptionInterceptor` and re-authorize per event, and that a bound directive on a subscription root field is a build error.

- [ ] **Step 2: Update CLAUDE.md**

Add to the Architecture section, in the style of the existing entries — stating what is load-bearing and what a green test does *not* prove:

> **Authorization is compiled, not wrapped.** `AuthShape` is built at plan
> compile and cached with the plan; it does not depend on the principal, so
> `planKey` is unaffected and the plan cache is not multiplied by policy. A
> field that declares nothing carries `authIdx == -1` and costs an integer
> compare. A bound directive on a subscription root field is rejected at
> `NewSchema`: it would wrap `fd.anyResolve`, and the per-event writer
> substitutes that executor outright, so the check would never run while every
> test stayed green.

- [ ] **Step 3: Run the full gate**

Run: `sh scripts/gate.sh -short`
Expected: `all modules pass`.

- [ ] **Step 4: Commit**

```bash
git add doc.go CLAUDE.md
git commit -m "docs: document the authorization surface

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Deferred to Plan 2

- **P4 `ObjectAuthorizer`** — per-object decisions batched through `WaveCoordinator`, and `SiteObject` population. Carries cases 12, 13, 15, 26 and the dynamic half of 28.
- **`Drop()`** — list-element omission; `validFor` rejects it until then.
- **`SiteFilterArg`** — the inference-leak check over `where`/`orderBy`/`groupBy`. Case 8.
- **`SiteInputWrite`** — the readonly-input walk, coordinates at compile and values at decision time. Case 9.
- **P6 introspection filtering** — opt-in, requires making `introType.fields` context-aware. Case 19.
- **Static grant folding** — `AuthSite.Grants` is declared and always empty until then. Case 28's static half.
- **`ext/authz`** — the Apollo vocabulary (`@authenticated`, `@requiresScopes`, `@policy`) and a batched `Guard` over `loader.Loader`.

## Self-Review Notes

Checked against the spec:

- **Covered:** P1 (Tasks 3, 4), P2 (Task 5), P3 (Task 6, minus `Drop`), P5 (Task 7), P7 (Task 1), the §6 performance risks (Task 8), and the §7 deliberate-breakage discipline (Steps 7/8/6/5/6 of Tasks 1, 4, 6, 7).
- **Deliberately deferred:** P4, P6, and three of the four `SiteKind` populations, listed above. Each is named in the spec and none blocks the others.
- **Gap found and closed during review:** the spec's §7 requires a pin that `query { __schema { __typename } restrictedField }` is rejected. No task carried it; `TestComposedIntrospectionDoesNotBypassChecks` was added to Task 7.
- **Type consistency:** `Coord` is `string` throughout (deviation recorded). `authIdx` is `int32` in `planField` and converted at the one call site. `Decision.Set` returns `error`; `ScopeAuthorizer` propagates it.
- **Known unknowns, flagged inline rather than guessed:** `CodeForbidden`, `isLeafType`, `isIntrospectionType`, `toError`, and whether `plan_test.go` already has a plan helper. Each step says to check the file and reuse rather than assume the name.
