# Authorization 2c — instance sites Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a policy decide, per resolved object, whether a principal may see that row — allowing, nulling, denying, or dropping it from its list — with the checks batched one call per wave.

**Architecture:** `@authorizeObject` marks a type instance-guarded at `NewSchema`. Plan compile records a `SiteInstance` site for every field position that can return such a type, stored contiguously after the field's output and argument sites, so `planField` neither grows nor gains a field. At execution an optional `ObjectAuthorizer` is called once per drained list (split at a batch size), and its per-element `Outcome` is applied as the element is written; `Drop` omits the element.

**Tech Stack:** Go 1.27, `gqlparser/v2`, the root `graphql` package. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-16-authorization-design.md` §9.5, decisions D1–D12 (binding), with §9.2 and §9.4 for the 2a/2b machinery this builds on.

## Global Constraints

- Root package may depend only on `gqlparser/v2` and the standard library.
- No reflection on the request hot path.
- `execState` stays 64 bytes, `OperationContext` 160 bytes, `planField` 176 bytes (`TestStructSizes`). A field with no site keeps `authIdx == -1` and pays one integer compare.
- Schema mismatches are reported at `NewSchema` as joined errors, never at request time.
- `AuthShape` is principal-independent, built once per plan and shared by concurrent requests; per-request state never touches it.
- Effective requirements are read from `fieldDef.requires` / `objectType.requires`, never recomputed from `requirementOf`.
- Tests live beside the code in `package graphql` and reuse `fixture_test.go` helpers where they fit.
- Comments explain why, not what. English only. No plan or task numbers in code.
- Commit messages: imperative, lower-case type prefix. Stage files explicitly; never `git add -A`; never `git stash`.
- Gate for every task: `go vet ./... && go test -race -count=1 ./...`, plus `sh scripts/gate.sh -short` for tasks that change the public API.

---

### Task 1: Public surface — `SiteInstance`, `ObjectAuthorizer`, options, outcome validity

**Files:**
- Modify: `authz.go` (SiteKind block ~line 122, `validFor` ~line 304, options ~line 379)
- Test: `authz_instance_test.go` (create)

**Interfaces:**
- Consumes: `Outcome`, `AuthSite`, `ExecutorOption`, `Errorf`.
- Produces: `SiteInstance SiteKind`; `type ObjectCheck struct{ Site AuthSite; Type string; Object any }`; `type ObjectAuthorizer interface{ AuthorizeObjects(ctx context.Context, checks []ObjectCheck) ([]Outcome, error) }`; `WithObjectAuthorizer(ObjectAuthorizer) ExecutorOption`; `WithObjectAuthBatch(int) ExecutorOption`; `Executor.objectAuthorizer`, `Executor.objectAuthBatch` (default 50).

- [ ] **Step 1: Write the failing tests**

In `authz_instance_test.go`:

```go
package graphql

import "testing"

func TestObjectAuthBatchDefaultsAndOverrides(t *testing.T) {
	s := newFixtureSchema(t)
	e := NewExecutor(s)
	if e.objectAuthBatch != 50 {
		t.Fatalf("default batch = %d, want 50", e.objectAuthBatch)
	}
	e = NewExecutor(s, WithObjectAuthBatch(7))
	if e.objectAuthBatch != 7 {
		t.Fatalf("batch = %d, want 7", e.objectAuthBatch)
	}
	// A non-positive size is the caller asking for no splitting at all, which
	// would defeat the bound: keep the default rather than silently accepting.
	e = NewExecutor(s, WithObjectAuthBatch(0))
	if e.objectAuthBatch != 50 {
		t.Fatalf("batch = %d, want 50 for a non-positive size", e.objectAuthBatch)
	}
}

func TestInstanceSiteAdmitsOnlyAllowNullDenyDrop(t *testing.T) {
	site := AuthSite{Coord: "Customer", Kind: SiteInstance}
	for _, o := range []Outcome{Allow(), Null(), Deny("read", "Customer"), Drop()} {
		if err := o.validFor(site); err != nil {
			t.Fatalf("outcome rejected for an instance site: %v", err)
		}
	}
	for name, o := range map[string]Outcome{
		"Zero":   Zero(),
		"Redact": Redact(func(v any) any { return v }),
	} {
		err := o.validFor(site)
		if err == nil {
			t.Fatalf("%s accepted for an instance site", name)
		}
		if !contains(err.Error(), "Customer") {
			t.Fatalf("%s error does not name the coordinate: %v", name, err)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

If `fixture_test.go` exposes a schema constructor under a different name than `newFixtureSchema`, use that one; if it only exposes `newFixtureExecutor`, build the executor with it and read the fields from it instead. Prefer `strings.Contains` over the helpers above if importing `strings` is cleaner — the helpers exist only so the test does not depend on an import the file may not otherwise need.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestObjectAuth|TestInstanceSite' .`
Expected: FAIL — `undefined: WithObjectAuthBatch`, `undefined: SiteInstance`.

- [ ] **Step 3: Implement**

In `authz.go`, extend the `SiteKind` block:

```go
	// SiteInstance is one resolved object of an @authorizeObject type, about
	// to be written at this field position. Unlike every other site it is
	// decided during execution by an ObjectAuthorizer, because the values do
	// not exist when a Decision is built.
	SiteInstance
```

Add beside `Authorizer`:

```go
// ObjectCheck is one resolved object an ObjectAuthorizer decides on. Type is
// the concrete object type's name, which differs from Site.Coord when the
// field's declared type is an interface or a union.
type ObjectCheck struct {
	Site   AuthSite
	Type   string
	Object any
}

// ObjectAuthorizer decides whether a principal may see individual objects.
// It is called once per wave with every guarded value in it, so a policy
// backed by a remote decision point issues one batched call rather than one
// per row. The returned slice is positional and must have the same length as
// checks; any other length is an error, because a short slice would silently
// allow the rows it does not cover.
type ObjectAuthorizer interface {
	AuthorizeObjects(ctx context.Context, checks []ObjectCheck) ([]Outcome, error)
}

// WithObjectAuthorizer enables instance-level authorization. Without it,
// @authorizeObject describes positions and nothing is enforced.
func WithObjectAuthorizer(a ObjectAuthorizer) ExecutorOption {
	return func(e *Executor) { e.objectAuthorizer = a }
}

// WithObjectAuthBatch bounds how many checks one AuthorizeObjects call
// carries; larger waves are split into sequential calls. The default of 50
// matches OpenFGA's BatchCheck default, which is the shape most remote
// policy backends are tuned for.
func WithObjectAuthBatch(n int) ExecutorOption {
	return func(e *Executor) {
		if n > 0 {
			e.objectAuthBatch = n
		}
	}
}
```

In `exec.go`, add to `Executor` beside `authorizer`:

```go
	objectAuthorizer ObjectAuthorizer
	objectAuthBatch  int
```

and set `objectAuthBatch: 50` where `NewExecutor` fills its defaults (find the literal that already sets the other defaults; do not add a second initialization path).

In `validFor`, after the argument-site rule:

```go
	// An instance site decides whether one object may be seen. Zero and
	// Redact act on a leaf's value and have nothing to act on here.
	if site.Kind == SiteInstance && o.act != actionAllow && o.act != actionDeny &&
		o.act != actionNull && o.act != actionDrop {
		return Errorf("authorization: only Allow, Null, Deny and Drop are valid for %s, an instance site", site.Coord)
	}
```

`validFor`'s existing `actionNull` rule reads `site.Field.Type.NonNull`; an instance site records `Field` as nil (Task 3), so that rule does not fire here. Null on a non-null position bubbles at write time by the ordinary rules (spec D7).

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestObjectAuth|TestInstanceSite' .`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

Remove the `SiteInstance` clause from `validFor`, run the tests, and require `TestInstanceSiteAdmitsOnlyAllowNullDenyDrop` to FAIL for Zero and Redact. Restore it with a reverse edit — never `git checkout -- <file>`, which would discard the whole file's uncommitted work.

- [ ] **Step 6: Commit**

```bash
git add authz.go exec.go authz_instance_test.go
git commit -m "feat: declare instance sites and the ObjectAuthorizer surface

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `@authorizeObject` placement validation at `NewSchema`

**Files:**
- Modify: `authz_shape.go` (beside `validateInputDirectives`), `schema.go` (build phase 6 call site), `schema.go` `objectType` (~line 110)
- Test: `authz_instance_test.go`

**Interfaces:**
- Consumes: `schemaBuilder.errorf`, `coordinate`, `argCoordinate`, `b.ast.Types`.
- Produces: `const objectDirective = "authorizeObject"`; `(*schemaBuilder).validateObjectDirectives()`; `objectType.instanceGuarded bool`.

- [ ] **Step 1: Write the failing tests**

```go
const authzObjectSDL = `
directive @authorizeObject on OBJECT
type Customer @authorizeObject { id: ID! name: String! }
type Query { customers: [Customer!]! }
`

func TestAuthorizeObjectPlacementAtBuild(t *testing.T) {
	cases := []struct {
		name    string
		sdl     string
		wantErr string
	}{
		{
			name: "on an object type",
			sdl:  authzObjectSDL,
		},
		{
			name:    "on an interface",
			sdl:     "directive @authorizeObject on OBJECT | INTERFACE\ninterface Node @authorizeObject { id: ID! }\ntype Customer implements Node { id: ID! }\ntype Query { c: Customer! }",
			wantErr: "Node: @authorizeObject is valid only on an object type, not an interface",
		},
		{
			name:    "on a field",
			sdl:     "directive @authorizeObject on OBJECT | FIELD_DEFINITION\ntype Query { c: String @authorizeObject }",
			wantErr: "Query.c: @authorizeObject is valid only on an object type, not a field",
		},
		{
			name:    "on an input object",
			sdl:     "directive @authorizeObject on OBJECT | INPUT_OBJECT\ninput Where @authorizeObject { name: String }\ntype Query { c(w: Where): String }",
			wantErr: "Where: @authorizeObject is valid only on an object type, not an input object",
		},
		{
			name:    "on a union",
			sdl:     "directive @authorizeObject on OBJECT | UNION\ntype A { id: ID! }\ntype B { id: ID! }\nunion AB @authorizeObject = A | B\ntype Query { c: AB }",
			wantErr: "AB: @authorizeObject is valid only on an object type, not a union",
		},
		{
			name:    "more than once on one type",
			sdl:     "directive @authorizeObject repeatable on OBJECT\ntype Customer @authorizeObject @authorizeObject { id: ID! }\ntype Query { c: Customer }",
			wantErr: "Customer: @authorizeObject must not occur more than once",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(c.sdl)
			if c.wantErr == "" {
				if err != nil && indexOf(err.Error(), "authorizeObject") >= 0 {
					t.Fatalf("valid placement rejected: %v", err)
				}
				return
			}
			if err == nil || indexOf(err.Error(), c.wantErr) < 0 {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

func TestAuthorizeObjectMarksTheType(t *testing.T) {
	s, err := NewSchema(authzObjectSDL, Object[struct{}]("Customer"), Query(struct{}{}))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	if !s.objects["Customer"].instanceGuarded {
		t.Fatal("Customer is not marked instance-guarded")
	}
	if s.objects["Query"].instanceGuarded {
		t.Fatal("Query is marked instance-guarded")
	}
}
```

`TestAuthorizeObjectMarksTheType`'s bindings are illustrative: build the fixture the way `NewSchema` in this repo actually binds an object with two leaf fields (see `fixture_test.go` and `object.go`), keeping the SDL as written. Read `Schema`'s field holding built object types and use its real name if it is not `objects`.

The valid cases declare `@authorizeObject` with wider locations than D1 on purpose: with `on OBJECT` alone, gqlparser's own location check would reject them first and the test would prove nothing about this validator.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run TestAuthorizeObject .`
Expected: FAIL — no error is produced for the bad placements, and `instanceGuarded` is undefined.

- [ ] **Step 3: Implement**

In `authz_shape.go`:

```go
// objectDirective marks a type whose individual values an ObjectAuthorizer
// decides on.
const objectDirective = "authorizeObject"

// validateObjectDirectives rejects @authorizeObject anywhere the executor
// would not consult an ObjectAuthorizer. Every rejected location is one where
// a schema author could reasonably expect enforcement and get silence.
func (b *schemaBuilder) validateObjectDirectives() {
	reject := func(coord, what string, ds ast.DirectiveList) {
		if ds.ForName(objectDirective) != nil {
			b.errorf("%s: @%s is valid only on an object type, not %s", coord, objectDirective, what)
		}
	}
	for _, def := range b.ast.Types {
		switch def.Kind {
		case ast.Object:
			occ := def.Directives.ForNames(objectDirective)
			if len(occ) > 1 {
				// gqlparser merges an extension's directives into the base
				// list and skips its own non-repeatable check for them, so a
				// second occurrence reaches here even when the directive is
				// not declared repeatable.
				b.errorf("%s: @%s must not occur more than once on one type", def.Name, objectDirective)
			}
			for _, f := range def.Fields {
				reject(coordinate(def.Name, f.Name), "a field", f.Directives)
				for _, a := range f.Arguments {
					reject(argCoordinate(coordinate(def.Name, f.Name), a.Name), "a field argument", a.Directives)
				}
			}
		case ast.Interface:
			reject(def.Name, "an interface", def.Directives)
		case ast.Union:
			reject(def.Name, "a union", def.Directives)
		case ast.InputObject:
			reject(def.Name, "an input object", def.Directives)
			for _, f := range def.Fields {
				reject(coordinate(def.Name, f.Name), "an input field", f.Directives)
			}
		case ast.Enum:
			reject(def.Name, "an enum", def.Directives)
			for _, v := range def.EnumValues {
				reject(coordinate(def.Name, v.Name), "an enum value", v.Directives)
			}
		case ast.Scalar:
			reject(def.Name, "a scalar", def.Directives)
		}
	}
	for _, d := range b.ast.Directives {
		for _, a := range d.Arguments {
			reject(argCoordinate("@"+d.Name, a.Name), "a directive argument", a.Directives)
		}
	}
	if b.ast.Schema != nil {
		reject("schema", "the schema definition", b.ast.Schema.Directives)
	}
}
```

Match the surrounding style: `validateAuthDirectives` and `validateInputDirectives` already walk these locations, so mirror whichever loop shape they use rather than inventing a third.

Call it from `schema.go`'s build phase 6, immediately after `validateInputDirectives()`.

In `schema.go`, add to `objectType`:

```go
	// instanceGuarded reports that the type carries @authorizeObject, so a
	// value of it is offered to the ObjectAuthorizer before being written.
	instanceGuarded bool
```

and set it where the object shell is built, from `def.Directives.ForName(objectDirective) != nil`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run TestAuthorizeObject .`
Expected: PASS (6 subtests plus the marking test).

- [ ] **Step 5: Prove it can fail**

Delete the `ast.Union` case, run, require the union subtest to FAIL. Restore. Then set `instanceGuarded` to a constant `false`, run, require `TestAuthorizeObjectMarksTheType` to FAIL. Restore.

- [ ] **Step 6: Gate and commit**

```bash
go vet ./... && go test -race -count=1 ./...
git add authz_shape.go schema.go authz_instance_test.go
git commit -m "feat: validate @authorizeObject placement at NewSchema

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Instance sites at plan compile, with the derived index

**Files:**
- Modify: `plan.go` (`planField.argSites` ~line 100), `authz_shape.go` (`shapeBuilder.field`), `authz.go` (`AuthShape`)
- Test: `authz_instance_test.go`, `authz_bench_test.go` (`TestStructSizes`)

**Interfaces:**
- Consumes: `planField.authIdx`, `planField.argSites`, `buildAuthShape`, `objectType.instanceGuarded`, `abstractType.possible`.
- Produces: `instanceSiteBit`, `argSiteMask`; `(*planField).argSiteCount() int32`, `(*planField).hasInstanceSite() bool`, `(*planField).instanceIdx() int32`; `AuthShape.hasInstanceSites bool`.

- [ ] **Step 1: Write the failing tests**

```go
func TestInstanceSitesAtPlanCompile(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ customers { id } plain }`)

	f := fieldByName(t, p, "customers")
	if f.authIdx < 0 {
		t.Fatal("a field returning a guarded type must route through enforceAuth")
	}
	if !f.hasInstanceSite() {
		t.Fatal("customers has no instance site")
	}
	if f.argSiteCount() != 0 {
		t.Fatalf("argSiteCount = %d, want 0", f.argSiteCount())
	}
	site := p.shape.sites[f.instanceIdx()]
	if site.Kind != SiteInstance || site.Coord != "Customer" {
		t.Fatalf("site = %v/%q, want SiteInstance/Customer", site.Kind, site.Coord)
	}
	if !site.Requires.IsZero() {
		t.Fatal("an instance site carries no requirement of its own")
	}
	if site.Field != nil {
		t.Fatal("an instance site has no field of its own")
	}
	if out := p.shape.sites[f.authIdx]; out.Kind != SiteOutput || !out.Requires.IsZero() {
		t.Fatalf("routing site = %v, want a zero-requirement SiteOutput", out.Kind)
	}

	plain := fieldByName(t, p, "plain")
	if plain.authIdx != -1 || plain.hasInstanceSite() {
		t.Fatalf("plain: authIdx=%d hasInstanceSite=%v, want -1/false", plain.authIdx, plain.hasInstanceSite())
	}
	if !p.shape.hasInstanceSites {
		t.Fatal("shape does not record instance sites")
	}
}

func TestInstanceSiteFollowsArgumentSitesContiguously(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ filtered(where: {nameContains: "a"}) { id } }`)
	f := fieldByName(t, p, "filtered")
	if f.argSiteCount() != 1 || !f.hasInstanceSite() {
		t.Fatalf("argSiteCount=%d hasInstanceSite=%v, want 1/true", f.argSiteCount(), f.hasInstanceSite())
	}
	if k := p.shape.sites[f.authIdx+1].Kind; k != SiteFilterArg {
		t.Fatalf("site after the output site = %v, want SiteFilterArg", k)
	}
	if k := p.shape.sites[f.instanceIdx()].Kind; k != SiteInstance {
		t.Fatalf("site at instanceIdx = %v, want SiteInstance", k)
	}
	if f.instanceIdx() != f.authIdx+2 {
		t.Fatalf("instanceIdx = %d, want %d", f.instanceIdx(), f.authIdx+2)
	}
}

func TestInstanceSiteOnAnAbstractPosition(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ node { __typename } }`)
	f := fieldByName(t, p, "node")
	if !f.hasInstanceSite() {
		t.Fatal("an abstract position with a guarded implementer needs an instance site")
	}
	if c := p.shape.sites[f.instanceIdx()].Coord; c != "Node" {
		t.Fatalf("Coord = %q, want the abstract type's name Node", c)
	}
}

func TestPlanWithoutInstanceSitesIsUnchanged(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ plain }`)
	if p.shape != nil {
		t.Fatalf("shape = %v, want nil for a plan that touches nothing guarded", p.shape)
	}
}
```

Fixture and helpers, in the same file:

```go
const instanceSDL = `
directive @authorizeObject on OBJECT
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
input Where { nameContains: String }
interface Node { id: ID! }
type Customer implements Node @authorizeObject { id: ID! name: String! }
type Open implements Node { id: ID! }
type Query {
  customers: [Customer!]!
  maybe: Customer
  filtered(where: Where @authorizeInput(kind: FILTER)): [Customer!]!
  node: Node
  plain: String!
}
`
```

`newInstanceExecutor` builds an executor over `instanceSDL` with `Customer`, `Open` and `Query` bound; `planFor` compiles a document to a `*plan`; `fieldByName` finds a root `*planField` by response key. Reuse whatever the 2b tests already do for these three — `authz_input_test.go` has the same needs — rather than writing new ones; if those helpers are unexported and named differently, use the existing names.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestInstanceSite|TestPlanWithoutInstanceSites' .`
Expected: FAIL — `hasInstanceSite` undefined.

- [ ] **Step 3: Implement**

In `plan.go`, replace the `argSites` field's comment and add the helpers below the struct:

```go
	// argSites packs two things into the word beside authIdx, because
	// planField is exactly full at 176 bytes: an added int32 or even an added
	// bool measures 184, crossing a size class that runSubscriptionEvent pays
	// per event (`f := *src` copies a planField). The low bits count this
	// field's argument sites and instanceSiteBit records that an instance
	// site follows them. The plan's AuthShape stores a field's sites
	// contiguously -- output site, argument sites, instance site -- so both
	// are indices derived from authIdx rather than stored.
	argSites int32
}

const (
	// instanceSiteBit rides above any plausible argument count: a field's
	// arguments are bounded by its SDL definition.
	instanceSiteBit int32 = 1 << 30
	argSiteMask     int32 = instanceSiteBit - 1
)

func (f *planField) argSiteCount() int32 { return f.argSites & argSiteMask }

func (f *planField) hasInstanceSite() bool { return f.argSites&instanceSiteBit != 0 }

// instanceIdx is valid only when hasInstanceSite reports true.
func (f *planField) instanceIdx() int32 { return f.authIdx + 1 + f.argSiteCount() }
```

In `authz_exec.go`, change `enforceAuth`'s argument loop bound from `f.argSites` to `f.argSiteCount()`. Search the package for every other read of `.argSites` and route it through the helpers; the raw field must be read only by them and by the shape builder that writes it.

In `authz_shape.go`'s `shapeBuilder.field`, after the argument-site loop, before `b.walk`:

```go
	if f.def != nil && b.instanceGuardedAt(f) {
		if f.authIdx < 0 {
			f.authIdx = int32(len(b.sites))
			b.sites = append(b.sites, AuthSite{
				Coord:  coordinate(f.def.object.name, f.name),
				Field:  f.def.def,
				Object: f.def.object.def,
				Kind:   SiteOutput,
				leaf:   f.def.leaf,
			})
		}
		b.sites = append(b.sites, AuthSite{
			Coord: f.def.def.Type.Name(),
			Kind:  SiteInstance,
		})
		f.argSites |= instanceSiteBit
		b.hasInstanceSites = true
	}
```

The zero-Requires output site is the same routing trick 2b uses, and it must be appended *before* any argument site when the field has both — it already is, because 2b appends it ahead of its argument loop. The instance site is appended last, which is what makes `instanceIdx` correct.

```go
// instanceGuardedAt reports whether f can return a value an ObjectAuthorizer
// decides on. An abstract position is guarded when any implementer is, since
// the concrete type is known only once the value resolves.
func (b *shapeBuilder) instanceGuardedAt(f *planField) bool {
	if f.target != nil {
		return f.target.instanceGuarded
	}
	if f.abstract == nil {
		return false
	}
	for _, obj := range f.abstract.possible {
		if obj.instanceGuarded {
			return true
		}
	}
	return false
}
```

Add `hasInstanceSites bool` to `shapeBuilder` and to `AuthShape`, and carry it in `buildAuthShape`'s returned literal beside `hasArgSites`.

In `authz_bench_test.go`'s `TestStructSizes`, keep the existing 176-byte assertion and add:

```go
	if argSiteMask&instanceSiteBit != 0 {
		t.Fatal("the instance bit overlaps the argument-site count")
	}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestInstanceSite|TestPlanWithout|TestStructSizes|TestArgumentSite' .`
Expected: PASS, including 2b's argument-site tests, which now read the packed count.

- [ ] **Step 5: Prove it can fail**

(a) Change `instanceIdx` to `f.authIdx + 1` (dropping the argument count). `TestInstanceSiteFollowsArgumentSitesContiguously` must FAIL. Revert.
(b) Make `instanceGuardedAt` return `f.target != nil && f.target.instanceGuarded` only. `TestInstanceSiteOnAnAbstractPosition` must FAIL. Revert.
(c) Add an `int32` field to `planField`. `TestStructSizes` must FAIL at 184 bytes. Revert.

- [ ] **Step 6: Gate and commit**

```bash
go vet ./... && go test -race -count=1 ./...
git add plan.go authz.go authz_shape.go authz_exec.go authz_bench_test.go authz_instance_test.go
git commit -m "feat: record instance sites at plan compile

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: The batched call — `authz_instance.go`

**Files:**
- Create: `authz_instance.go`
- Test: `authz_instance_test.go`

**Interfaces:**
- Consumes: `Executor.objectAuthorizer`, `Executor.objectAuthBatch`, `Executor.recover`, `authorizerError`, `panicError`, `Outcome.validFor`, `AuthShape.sites`.
- Produces: `(*execState).checkObjects(ctx context.Context, site AuthSite, checks []ObjectCheck) ([]Outcome, error)`.

This task builds the call and its error handling with no executor wiring; Task 5 calls it.

- [ ] **Step 1: Write the failing tests**

```go
type objectAuthorizerFunc func(ctx context.Context, checks []ObjectCheck) ([]Outcome, error)

func (f objectAuthorizerFunc) AuthorizeObjects(ctx context.Context, checks []ObjectCheck) ([]Outcome, error) {
	return f(ctx, checks)
}

func TestCheckObjectsSplitsAtTheBatchSize(t *testing.T) {
	var sizes []int
	a := objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		sizes = append(sizes, len(checks))
		out := make([]Outcome, len(checks))
		for i := range out {
			out[i] = Null()
		}
		return out, nil
	})
	st := newInstanceState(t, a, WithObjectAuthBatch(2))
	checks := make([]ObjectCheck, 5)
	outs, err := st.checkObjects(context.Background(), AuthSite{Coord: "Customer", Kind: SiteInstance}, checks)
	if err != nil {
		t.Fatalf("checkObjects: %v", err)
	}
	if len(outs) != 5 {
		t.Fatalf("len(outs) = %d, want 5", len(outs))
	}
	if len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("batch sizes = %v, want [2 2 1]", sizes)
	}
	for i, o := range outs {
		if o.act != actionNull {
			t.Fatalf("outs[%d] = %v, want Null", i, o.act)
		}
	}
}

func TestCheckObjectsRejectsAShortResult(t *testing.T) {
	a := objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		return make([]Outcome, len(checks)-1), nil
	})
	st := newInstanceState(t, a)
	_, err := st.checkObjects(context.Background(), AuthSite{Coord: "Customer", Kind: SiteInstance}, make([]ObjectCheck, 3))
	if err == nil {
		t.Fatal("a short result was accepted, which would silently allow the rows it does not cover")
	}
}

func TestCheckObjectsRejectsAnInvalidOutcome(t *testing.T) {
	a := objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		return []Outcome{Zero()}, nil
	})
	st := newInstanceState(t, a)
	_, err := st.checkObjects(context.Background(), AuthSite{Coord: "Customer", Kind: SiteInstance}, make([]ObjectCheck, 1))
	if err == nil || indexOf(err.Error(), "instance site") < 0 {
		t.Fatalf("err = %v, want the validFor rejection", err)
	}
}

func TestCheckObjectsHidesABackendFailure(t *testing.T) {
	a := objectAuthorizerFunc(func(_ context.Context, _ []ObjectCheck) ([]Outcome, error) {
		return nil, errors.New("dial tcp 10.0.3.7:8181: connection refused")
	})
	st := newInstanceState(t, a)
	_, err := st.checkObjects(context.Background(), AuthSite{Coord: "Customer", Kind: SiteInstance}, make([]ObjectCheck, 1))
	if err == nil {
		t.Fatal("no error")
	}
	if indexOf(err.Error(), "10.0.3.7") >= 0 {
		t.Fatalf("the backend's address reached the client: %v", err)
	}
}

func TestCheckObjectsRecoversAPanic(t *testing.T) {
	a := objectAuthorizerFunc(func(_ context.Context, _ []ObjectCheck) ([]Outcome, error) {
		panic("policy exploded")
	})
	st := newInstanceState(t, a)
	_, err := st.checkObjects(context.Background(), AuthSite{Coord: "Customer", Kind: SiteInstance}, make([]ObjectCheck, 1))
	if err == nil {
		t.Fatal("a panic in the ObjectAuthorizer was not recovered")
	}
	if indexOf(err.Error(), "policy exploded") >= 0 {
		t.Fatalf("the panic value reached the client: %v", err)
	}
}
```

`newInstanceState(t, a, opts...)` builds an executor over `instanceSDL` with `WithObjectAuthorizer(a)` and the given options, and returns an `*execState` for it. Build it the way the existing tests build one — `authz_input_test.go` and `exec.go` show which fields an `execState` needs; if constructing one directly is awkward, give `checkObjects` a receiver of `*Executor` instead and adjust Task 5's call sites to match, noting the change in the report.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run TestCheckObjects .`
Expected: FAIL — `checkObjects` undefined.

- [ ] **Step 3: Implement**

Create `authz_instance.go`:

```go
package graphql

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// checkObjects asks the ObjectAuthorizer about one wave's worth of values,
// splitting at the configured batch size. The result is positional: outs[i]
// is the outcome for checks[i].
//
// A failure fails every check in the wave rather than allowing the rest: a
// policy backend that is down must not be the reason a row becomes visible.
func (st *execState) checkObjects(ctx context.Context, site AuthSite, checks []ObjectCheck) (outs []Outcome, err error) {
	a := st.e.objectAuthorizer
	if a == nil || len(checks) == 0 {
		return nil, nil
	}
	if st.e.recover {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "graphql: object authorizer panic",
					"panic", r,
					"stack", string(debug.Stack()),
				)
				outs, err = nil, st.e.authorizerError(ctx, &panicError{value: r})
			}
		}()
	}
	outs = make([]Outcome, 0, len(checks))
	for start := 0; start < len(checks); start += st.e.objectAuthBatch {
		end := min(start+st.e.objectAuthBatch, len(checks))
		batch, berr := a.AuthorizeObjects(ctx, checks[start:end])
		if berr != nil {
			return nil, st.e.authorizerError(ctx, berr)
		}
		if len(batch) != end-start {
			return nil, st.e.authorizerError(ctx, Errorf("authorization: ObjectAuthorizer returned %d outcomes for %d checks at %s", len(batch), end-start, site.Coord))
		}
		for _, o := range batch {
			if verr := o.validFor(site); verr != nil {
				return nil, st.e.authorizerError(ctx, verr)
			}
		}
		outs = append(outs, batch...)
	}
	return outs, nil
}
```

`authorizerError` is `exec.go`'s existing hardening: it keeps an `*Error` as-is and replaces anything else with a generic internal error carrying the original behind `authorizerCause`. Read it before using it, and if its signature differs, adapt the calls rather than duplicating its logic here.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run TestCheckObjects .`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

(a) Drop the length check. `TestCheckObjectsRejectsAShortResult` must FAIL. Revert.
(b) Return `berr` unwrapped. `TestCheckObjectsHidesABackendFailure` must FAIL. Revert.
(c) Replace the split loop with one call over all checks. `TestCheckObjectsSplitsAtTheBatchSize` must FAIL. Revert.

- [ ] **Step 6: Commit**

```bash
git add authz_instance.go authz_instance_test.go
git commit -m "feat: batch instance checks with hardened failures

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Enforcement — a single object, a list, and `Drop`

**Files:**
- Modify: `exec_object.go` (`writeValue` ~line 224, `writeList` ~line 288, `writeListConcurrent` ~line 463), `authz.go` (`Drop` doc, remove the "not yet implemented" rejection in `validFor`)
- Test: `authz_instance_test.go`

**Interfaces:**
- Consumes: `(*execState).checkObjects`, `planField.hasInstanceSite/instanceIdx`, `objectType.instanceGuarded`, `valueShape.traverse`, `writeNullValue`.
- Produces: enforcement behaviour only; no new exported names.

- [ ] **Step 1: Write the failing tests**

```go
func TestInstanceOutcomesOnASingleObject(t *testing.T) {
	cases := []struct {
		name     string
		outcome  Outcome
		query    string
		wantData string
		wantErr  string
	}{
		{name: "allow", outcome: Allow(), query: `{ maybe { id } }`, wantData: `{"maybe":{"id":"c1"}}`},
		{name: "null", outcome: Null(), query: `{ maybe { id } }`, wantData: `{"maybe":null}`},
		{name: "deny", outcome: Deny("customer:read", "Customer"), query: `{ maybe { id } }`, wantData: `{"maybe":null}`, wantErr: "denied"},
		{name: "drop outside a list", outcome: Drop(), query: `{ maybe { id } }`, wantData: `{"maybe":null}`, wantErr: "Drop is valid only for a list element"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newInstanceExecutorWith(t, constantObjectPolicy(c.outcome))
			resp := run(t, e, c.query, nil)
			assertJSON(t, resp.Data, c.wantData)
			assertErrorContains(t, resp.Errors, c.wantErr)
		})
	}
}

func TestInstanceDropOmitsListElements(t *testing.T) {
	// customers resolves c1, c2, c3; the policy drops c2.
	e := newInstanceExecutorWith(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		outs := make([]Outcome, len(checks))
		for i, c := range checks {
			if idOf(c.Object) == "c2" {
				outs[i] = Drop()
			}
		}
		return outs, nil
	}))
	resp := run(t, e, `{ customers { id } }`, nil)
	assertJSON(t, resp.Data, `{"customers":[{"id":"c1"},{"id":"c3"}]}`)
	if len(resp.Errors) != 0 {
		t.Fatalf("errors = %v, want none: a dropped row leaves no trace", resp.Errors)
	}
}

func TestInstanceDenyInAListReportsTheWrittenIndex(t *testing.T) {
	// Drop c1, deny c2: the denied element is written at index 0, because
	// dropping renumbers what follows it.
	e := newInstanceExecutorWith(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		outs := make([]Outcome, len(checks))
		for i, c := range checks {
			switch idOf(c.Object) {
			case "c1":
				outs[i] = Drop()
			case "c2":
				outs[i] = Deny("customer:read", "Customer")
			}
		}
		return outs, nil
	}))
	resp := run(t, e, `{ customers { id } }`, nil)
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", resp.Errors)
	}
	assertPath(t, resp.Errors[0].Path, Path{{Key: "customers"}, {Index: 0}})
}

func TestInstanceNullBubblesThroughANonNullElement(t *testing.T) {
	// customers is [Customer!]!, so Null on an element nulls the whole field
	// by the ordinary rules -- which is why Drop exists.
	e := newInstanceExecutorWith(t, constantObjectPolicy(Null()))
	resp := run(t, e, `{ customers { id } }`, nil)
	assertJSON(t, resp.Data, `null`)
	assertErrorContains(t, resp.Errors, "non-nullable")
}

func TestInstanceChecksAreBatchedPerList(t *testing.T) {
	var calls, seen int
	e := newInstanceExecutorWith(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		calls++
		seen += len(checks)
		return make([]Outcome, len(checks)), nil
	}))
	run(t, e, `{ customers { id } }`, nil)
	if calls != 1 || seen != 3 {
		t.Fatalf("calls=%d checks=%d, want 1 call carrying 3 checks", calls, seen)
	}
}

func TestInstanceCheckCarriesTheConcreteType(t *testing.T) {
	var got ObjectCheck
	e := newInstanceExecutorWith(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		got = checks[0]
		return make([]Outcome, len(checks)), nil
	}))
	run(t, e, `{ node { __typename } }`, nil)
	if got.Type != "Customer" || got.Site.Coord != "Node" {
		t.Fatalf("check = type %q at %q, want Customer at Node", got.Type, got.Site.Coord)
	}
	if got.Object == nil {
		t.Fatal("the check carries no object")
	}
}

func TestUnguardedTypeInAnAbstractPositionIsNotChecked(t *testing.T) {
	// node resolves an Open, which carries no @authorizeObject.
	var calls int
	e := newInstanceExecutorOpenNode(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		calls++
		return make([]Outcome, len(checks)), nil
	}))
	run(t, e, `{ node { __typename } }`, nil)
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 for an unguarded concrete type", calls)
	}
}

func TestNoObjectAuthorizerEnforcesNothing(t *testing.T) {
	e := newInstanceExecutor(t) // no WithObjectAuthorizer
	resp := run(t, e, `{ customers { id } }`, nil)
	assertJSON(t, resp.Data, `{"customers":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`)
}
```

Helpers to add beside them: `constantObjectPolicy(o Outcome)` returns an `objectAuthorizerFunc` answering `o` for every check; `idOf(v any)` reads the fixture object's id; `newInstanceExecutorWith(t, a)` is `newInstanceExecutor` plus `WithObjectAuthorizer(a)`; `newInstanceExecutorOpenNode` binds `node` to an `Open` value instead of a `Customer`. `run`, `assertJSON`, `assertErrorContains` and `assertPath` are the fixture helpers if they exist under those names — check `fixture_test.go` first and use the real ones, adapting these assertions to their signatures.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestInstance|TestNoObjectAuthorizer|TestUnguardedType' .`
Expected: FAIL — no checks are issued and `Drop` is still rejected as unimplemented.

- [ ] **Step 3: Implement**

In `authz.go`, delete `validFor`'s `case actionDrop: return Errorf("authorization: Drop is not yet implemented")` and update `Drop`'s doc comment to say it is valid only at a list element position and that dropping renumbers the indices that follow.

In `exec_object.go`, give the object path one entry point:

```go
// instanceOutcome asks the ObjectAuthorizer about one value about to be
// written at f. It returns the zero Outcome when nothing guards it, so the
// caller needs no nil check.
func (st *execState) instanceOutcome(ctx context.Context, f *planField, obj *objectType, v any) (Outcome, error) {
	if st.e.objectAuthorizer == nil || !f.hasInstanceSite() || !obj.instanceGuarded {
		return Outcome{}, nil
	}
	site := st.plan.shape.sites[f.instanceIdx()]
	outs, err := st.checkObjects(ctx, site, []ObjectCheck{{Site: site, Type: obj.name, Object: v}})
	if err != nil || len(outs) == 0 {
		return Outcome{}, err
	}
	return outs[0], nil
}
```

Use whatever `execState` already holds to reach the plan's shape — if it keeps the `*plan`, read `st.plan.shape`; if it keeps the shape directly, read that. Do not add a field to `execState`.

In `writeValue`, after the concrete object type is resolved and before `writeObject`:

```go
	o, err := st.instanceOutcome(ctx, f, obj, v)
	if err != nil {
		st.addFieldError(ctx, err, path, f.ast.Position)
		return !t.NonNull && writeNullOK(w)
	}
	switch o.act {
	case actionNull:
		return st.writeNullValue(ctx, w, t, f, path)
	case actionDeny:
		st.addFieldError(ctx, o.denial(), path, f.ast.Position)
		return st.writeNullValue(ctx, w, t, f, path) && !t.NonNull
	case actionDrop:
		st.addFieldError(ctx, Errorf("authorization: Drop is valid only for a list element, not at %s", f.def.coord()), path, f.ast.Position)
		return st.writeNullValue(ctx, w, t, f, path) && !t.NonNull
	}
```

Follow `enforceAuth`'s existing convention for "error recorded, value withheld" rather than inventing `writeNullOK`: read how `writeFieldValue` reports a withheld value and mirror it exactly, so null bubbling stays the existing code's job. Use the coordinate helper the file already uses for a field's `Type.field` name.

List batching: add one helper that both list paths call on a drained slice, before anything is written or spawned.

```go
// instanceOutcomes decides a whole drained list at once, which is what keeps
// a remote policy to one call per list rather than one per row. keep[i]
// reports whether elems[i] is still to be written; dropped elements leave no
// null, no error and no gap.
func (st *execState) instanceOutcomes(ctx context.Context, f *planField, shape *valueShape, elems []any) (outs []Outcome, err error) {
	if st.e.objectAuthorizer == nil || !f.hasInstanceSite() || len(elems) == 0 {
		return nil, nil
	}
	site := st.plan.shape.sites[f.instanceIdx()]
	checks := make([]ObjectCheck, 0, len(elems))
	at := make([]int, 0, len(elems))
	for i, e := range elems {
		obj, v, isNil, cerr := st.elementObject(f, shape, e)
		if cerr != nil || isNil || obj == nil || !obj.instanceGuarded {
			// A concrete type that is not guarded, an unresolvable one and a
			// nil element are all left to the ordinary write path, which
			// already reports what each of them means.
			continue
		}
		checks = append(checks, ObjectCheck{Site: site, Type: obj.name, Object: v})
		at = append(at, i)
	}
	if len(checks) == 0 {
		return nil, nil
	}
	batch, err := st.checkObjects(ctx, site, checks)
	if err != nil {
		return nil, err
	}
	outs = make([]Outcome, len(elems))
	for j, i := range at {
		outs[i] = batch[j]
	}
	return outs, nil
}
```

`elementObject` resolves one element to its concrete `*objectType` and pointer value, which is the same work `writeValue` does today between `f.target` and `concreteValue` — extract that into a helper and have `writeValue` call it too, rather than writing the logic twice.

Then in `writeList`, immediately after the elements are drained (drain them when the concurrent path did not, using `shape.traverse` as `writeListConcurrent` does) and before `w.BeginArray()`:

```go
	outs, aerr := st.instanceOutcomes(ctx, f, shape, drained)
	if aerr != nil {
		st.addFieldError(ctx, aerr, path, f.ast.Position)
		return false
	}
```

and drive the element loop from the outcomes, keeping a written index separate from the source index:

```go
	n := 0
	for i, e := range drained {
		switch outs[i].act { // outs is nil when nothing is guarded: index it through a helper that returns the zero Outcome for a nil slice
		case actionDrop:
			continue
		case actionDeny:
			st.addFieldError(ctx, outs[i].denial(), &pathNode{parent: path, index: n, isIndex: true}, f)
			if t.Elem.NonNull {
				failed = true
			} else {
				w.Null()
				n++
			}
			continue
		case actionNull:
			if t.Elem.NonNull {
				// The ordinary non-null rule: a null element nulls the list.
				failed = true
				continue
			}
			w.Null()
			n++
			continue
		}
		if !writeElem(n, e) {
			break
		}
		n++
	}
```

Keep `writeElem`'s existing body; only its index argument changes, so an error path reports the position the client actually sees. `failed` and the rewind that follows it stay exactly as they are.

`writeListConcurrent` calls `instanceOutcomes` on its drained slice before `pushWave`, drops the dropped elements from the slice it spawns, and announces the wave with the number of elements it actually spawns — otherwise a loader waits for tasks that will never begin. Denied and nulled elements still occupy a written position, so only `Drop` changes the count.

A list whose element type is itself a list recurses into `writeList`, which sees the same `f`, so nested lists are handled by the inner call and need no special case.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestInstance|TestNoObjectAuthorizer|TestUnguardedType|TestExec|TestLoader' .`
Expected: PASS, including the existing execution and loader suites.

- [ ] **Step 5: Prove it can fail**

(a) Ignore `actionDrop` in the list path (write the element instead). `TestInstanceDropOmitsListElements` must FAIL. Revert.
(b) Report the denial at the source index rather than the written one. `TestInstanceDenyInAListReportsTheWrittenIndex` must FAIL. Revert.
(c) Issue one call per element instead of per list. `TestInstanceChecksAreBatchedPerList` must FAIL. Revert.
(d) Check every object, guarded or not. `TestUnguardedTypeInAnAbstractPositionIsNotChecked` must FAIL. Revert.

- [ ] **Step 6: Gate and commit**

```bash
go vet ./... && go test -race -count=1 ./...
sh scripts/gate.sh -short
git add exec_object.go authz.go authz_instance.go authz_instance_test.go
git commit -m "feat: enforce instance outcomes and implement Drop

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Subscriptions, and the wave a loader shares

**Files:**
- Modify: `subscription.go` only if a per-event path bypasses `writeValue`
- Test: `authz_instance_test.go`

**Interfaces:**
- Consumes: Task 5's enforcement; `newAuthSubGuardedExecutor`-style fixtures from the 2a/2b tests.
- Produces: no new names.

- [ ] **Step 1: Write the failing tests**

```go
func TestSubscriptionEventInstanceDropAppliesPerEvent(t *testing.T) {
	var event int
	e := newInstanceSubExecutor(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		outs := make([]Outcome, len(checks))
		if event == 2 {
			for i, c := range checks {
				if idOf(c.Object) == "c2" {
					outs[i] = Drop()
				}
			}
		}
		return outs, nil
	}))

	ch, err := e.Subscribe(t.Context(), &Request{Query: `subscription { batches { id } }`})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	want := []string{
		`{"batches":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`,
		`{"batches":[{"id":"c1"},{"id":"c3"}]}`,
		`{"batches":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`,
	}
	for i, w := range want {
		event = i + 1
		resp, ok := <-ch
		if !ok {
			t.Fatalf("stream closed after %d events; a dropped row must not end it", i)
		}
		if len(resp.Errors) != 0 {
			t.Fatalf("event %d errors = %v, want none", i+1, resp.Errors)
		}
		assertJSON(t, resp.Data, w)
	}
}
```

`newInstanceSubExecutor` binds `subscription { batches: [Customer!]! }` to a source that yields the same three customers for each of three events, then closes; `event` is read inside the policy, so the source must publish one event at a time and the test must consume each before the next is produced — use the same hand-off the existing subscription tests use (a channel the test writes to), not a sleep.

The loader test goes in `loader/` (the root package's tests cannot import it), beside `TestLoaderResponseLimitDoesNotStrandWave`:

```go
func TestInstanceChecksShareTheWaveWithALoader(t *testing.T) {
	var batches, keys int
	// owner is resolved through a Loader for each of the three customers; one
	// batch proves the instance check did not split the wave.
	ld := loader.New(func(_ context.Context, ks []string) ([]string, []error) {
		batches++
		keys += len(ks)
		out := make([]string, len(ks))
		for i, k := range ks {
			out[i] = "owner-" + k
		}
		return out, nil
	})
	e := newGuardedLoaderExecutor(t, ld, allowEveryObject())
	resp := run(t, e, `{ customers { id owner } }`, nil)
	if len(resp.Errors) != 0 {
		t.Fatalf("errors = %v", resp.Errors)
	}
	if batches != 1 || keys != 3 {
		t.Fatalf("batches=%d keys=%d, want 1 batch of 3: the instance check fragmented the wave", batches, keys)
	}
}
```

`newGuardedLoaderExecutor` builds the `instanceSDL` schema with an extra `Customer.owner: String!` bound through the loader, plus `WithObjectAuthorizer`; `allowEveryObject()` answers the zero `Outcome` for every check. Match `loader/`'s existing test style for building an executor from that package — it already does this for the wave tests.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestSubscriptionEventInstance' .` and `go test -race -run TestInstanceChecksShareTheWave ./loader`
Expected: FAIL before Task 5's code is reachable from these paths, or PASS immediately — if a test passes without any change, say so in the report and strengthen it until removing Task 5's list path turns it red.

- [ ] **Step 3: Implement**

If both tests pass with no change, the per-event path already routes through `writeValue`/`writeList`, and this task's deliverable is the tests plus one line in the report saying so. If the per-event root substitution bypasses the check, apply the same `instanceOutcome` call there, mirroring how the subscription open handler applies argument denials.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -count=1 . ./loader`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

Set `f.argSites &^= instanceSiteBit` on the per-event planField copy in `runSubscriptionEvent`. `TestSubscriptionEventInstanceDropAppliesPerEvent` must FAIL. Revert.

- [ ] **Step 6: Commit**

```bash
git add subscription.go authz_instance_test.go loader/instance_wave_test.go
git commit -m "test: pin instance checks per subscription event and inside a wave

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

(Stage only the files that actually changed.)

---

### Task 7: Performance gate

**Files:** none committed unless a benchmark is added.

The plan's base commit is the branch point; the controller builds `before.exe` from it and places it in the plan's workspace directory.

- [ ] **Step 1: Interleave against the base**

```bash
WS=.superpowers/sdd/2026-09-18-authz-2c-instance-sites
BENCH='BenchmarkExecuteUsers|BenchmarkExecuteConcurrentList|BenchmarkExecuteTypenameHeavy'
go test -c -o "$WS/after.exe" .
rm -f "$WS/before.txt" "$WS/after.txt"
for i in $(seq 1 12); do
  "$WS/before.exe" -test.run xxx -test.bench "$BENCH" -test.benchmem -test.count=1 >> "$WS/before.txt"
  "$WS/after.exe"  -test.run xxx -test.bench "$BENCH" -test.benchmem -test.count=1 >> "$WS/after.txt"
done
benchstat "$WS/before.txt" "$WS/after.txt"
go test -run TestStructSizes -v .
```

**Acceptance:** no statistically significant regression on any of the three, allocs/op equal sample for sample, `execState` 64, `OperationContext` 160, `planField` 176. The three benchmarks touch no guarded type, so this measures exactly the claim that an unguarded plan pays nothing. If a regression appears, report the table and a diagnosis; do not tune.

- [ ] **Step 2: Report**

Put the benchstat table in the task report. No commit unless a benchmark file was added.

---

### Task 8: Documentation

**Files:**
- Modify: `doc.go` (authorization section), `CLAUDE.md` (the "Authorization is compiled, not wrapped." paragraph only), `docs/superpowers/specs/2026-09-16-authorization-design.md` (§9.5 Status and a Deviations block)

- [ ] **Step 1: `doc.go`**

Describe: `@authorizeObject`; `ObjectAuthorizer` and `WithObjectAuthorizer`; that checks are batched per wave and split at `WithObjectAuthBatch` (default 50); that Allow, Null, Deny and Drop are the valid outcomes and the zero `Outcome` allows; that `Drop` omits a list element, is invalid elsewhere, and renumbers the indices after it; that `Null` on a non-null element bubbles, which is why `Drop` exists; and that bulk row filtering belongs in the data source's query where it can express it — the batched hook is for what the query cannot. Remove the sentence saying `Drop` is defined but always rejected.

- [ ] **Step 2: `CLAUDE.md`**

Two or three sentences in that paragraph: instance sites ride the same `authIdx` compare through a zero-requirement output site; `planField` is exactly full at 176 bytes, so the instance site is found at `authIdx + 1 + argSiteCount()` with a flag bit in `argSites` rather than a new field (an added `int32` or `bool` both measure 184); and a batch failure fails every check in the wave rather than allowing the remainder.

- [ ] **Step 3: Spec**

§9.5 Status says 2c is implemented on its branch. Add a Deviations block listing everything that shipped beyond D1–D12, with the benchmark numbers from Task 7.

- [ ] **Step 4: Verify and commit**

Check every sentence against the code. Run `go vet ./... && sh scripts/gate.sh -short`, then:

```bash
git add doc.go CLAUDE.md docs/superpowers/specs/2026-09-16-authorization-design.md
git commit -m "docs: document instance-level authorization

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---
