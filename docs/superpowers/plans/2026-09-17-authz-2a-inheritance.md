# Authorization 2a: Requirement Inheritance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make object-level and interface-level `@requiresScopes` actually enforced, guard `__typename`, turn every unenforced placement into a build error, and stop Authorizer failures leaking to clients.

**Architecture:** A field's effective requirement — its own, its object type's, and each implemented interface's type-level and same-named field requirement — is computed once at `NewSchema` and stored on `fieldDef.requires`; an object's effective type-level requirement is stored on `objectType.requires`. The plan-compile shape builder and `RequireAuthCoverage` both read those stored values, so enforcement and coverage cannot diverge. `__typename` gets a `SiteObject` site. Nothing on the request path changes except that `__typename` now passes through the existing auth check.

**Tech Stack:** Go 1.27, `github.com/vektah/gqlparser/v2`.

**Spec:** `docs/superpowers/specs/2026-09-16-authorization-design.md` — §9.2 is this plan's design; §5 and §7 still bind.

## Global Constraints

- Root package may depend only on `github.com/vektah/gqlparser/v2` and the standard library.
- No reflection on the request hot path.
- Do not grow `execState` (64 bytes) or `OperationContext` (160 bytes). `fieldDef` and `objectType` are schema-level and may grow.
- Go 1.27. Tests live beside the code in `package graphql`.
- `-race` is not optional. The gate is `sh scripts/gate.sh -short` (four modules; `go test ./...` reaches one).
- Comments explain why, not what. English only. No code-narrating or history-narrating comments.
- Commit messages: imperative, lower-case type prefix (`feat:`, `fix:`, `test:`, `docs:`), ending with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Stage files explicitly; never `git add -A`. Never bare `git stash` / `git stash pop` — the stash stack is shared with other worktrees and live sessions.
- Every security-relevant check ships with a test proven to fail when the check is removed (spec §7). Record that failure in the task report.

## File Structure

| File | Change |
|---|---|
| `authz.go` | `Requirement.And`, `maxRequirementGroups`; `validFor` rule for `SiteObject`; `authorizerError`. |
| `authz_shape.go` | `resolveAuthRequirements` (build time); widened `validateAuthDirectives`; shape builder reads stored requirements and builds `SiteObject` for `__typename`; `validateAuthCoverage` reads stored requirements. |
| `object.go` | `fieldDef.requires Requirement`. |
| `schema.go` | `objectType.requires Requirement`; `build()` calls `resolveAuthRequirements`. |
| `plan.go` | `buildAuthShape(p.root, p.sel)`. |
| `exec_object.go` | `__typename` branch moves below the auth check; `writeField` treats `__typename` as non-null. |
| `exec.go`, `subscription.go` | `toError` replaced by `authorizerError`; `authorize` comment. |
| `authz_test.go`, `authz_inherit_test.go` (new) | Tests. `authz_inherit_test.go` holds everything this plan adds, so `authz_test.go` only changes where existing tests flip. |
| `authz_typename_bench_test.go` (new) | Interleaved-gate benchmark. |
| `doc.go`, `CLAUDE.md` | Limits that no longer apply are removed. |

---

### Task 1: `Requirement.And`

**Files:**
- Modify: `authz.go` (after `Scopes`)
- Create: `authz_inherit_test.go`

**Interfaces:**
- Produces: `func (r Requirement) And(o Requirement) Requirement`; `const maxRequirementGroups = 64`; `func (r Requirement) groupCount() int`.

- [ ] **Step 1: Write the failing test**

Create `authz_inherit_test.go`:

```go
package graphql

import (
	"slices"
	"testing"
)

func TestRequirementAnd(t *testing.T) {
	held := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	cases := []struct {
		name string
		got  Requirement
		want [][]string
	}{
		{"zero and x is x", Requirement{}.And(NewRequirement([]string{"a"})), [][]string{{"a"}}},
		{"x and zero is x", NewRequirement([]string{"a"}).And(Requirement{}), [][]string{{"a"}}},
		{"single groups merge", NewRequirement([]string{"a"}).And(NewRequirement([]string{"b"})), [][]string{{"a", "b"}}},
		{"cross product", NewRequirement([]string{"a"}, []string{"b"}).And(NewRequirement([]string{"c"})), [][]string{{"a", "c"}, {"b", "c"}}},
		{"duplicate scopes collapse", NewRequirement([]string{"a"}).And(NewRequirement([]string{"a"})), [][]string{{"a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !slices.EqualFunc(tc.got.anyOf, tc.want, slices.Equal[[]string]) {
				t.Errorf("anyOf = %v, want %v", tc.got.anyOf, tc.want)
			}
		})
	}

	// Semantics, not just shape: an AND must demand both sides.
	r := NewRequirement([]string{"a"}, []string{"b"}).And(NewRequirement([]string{"c"}))
	if r.Satisfied(held("a")) {
		t.Error("(a|b)&c satisfied by a alone")
	}
	if !r.Satisfied(held("b", "c")) {
		t.Error("(a|b)&c not satisfied by b,c")
	}
}

func TestRequirementAndDoesNotAliasItsInputs(t *testing.T) {
	a := NewRequirement([]string{"a"})
	b := NewRequirement([]string{"b"})
	r := a.And(b)
	r.anyOf[0][0] = "mutated"
	if a.anyOf[0][0] != "a" || b.anyOf[0][0] != "b" {
		t.Error("And shares backing arrays with its operands")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestRequirementAnd' .`
Expected: build failure, `r.And undefined`.

- [ ] **Step 3: Implement**

Append to `authz.go`:

```go
// maxRequirementGroups bounds an effective requirement after inheritance.
// AND-ing OR-of-AND requirements multiplies their group counts, so a field
// inheriting from an object and several interfaces can grow quickly; past this
// NewSchema fails, keeping a pathological combination a build error rather
// than a per-request cost.
const maxRequirementGroups = 64

// And returns the requirement satisfied only when both r and o are. It is the
// cross product of their groups, each group sorted and deduplicated. The zero
// Requirement admits everyone, so it is the identity.
func (r Requirement) And(o Requirement) Requirement {
	if r.IsZero() {
		return o
	}
	if o.IsZero() {
		return r
	}
	out := make([][]string, 0, len(r.anyOf)*len(o.anyOf))
	for _, a := range r.anyOf {
		for _, b := range o.anyOf {
			g := make([]string, 0, len(a)+len(b))
			g = append(g, a...)
			g = append(g, b...)
			slices.Sort(g)
			out = append(out, slices.Compact(g))
		}
	}
	return Requirement{anyOf: out}
}

func (r Requirement) groupCount() int { return len(r.anyOf) }
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race -run 'TestRequirement' .`
Expected: PASS (includes the pre-existing Requirement tests).

- [ ] **Step 5: Commit**

```bash
git add authz.go authz_inherit_test.go
git commit -m "feat: add Requirement.And for inherited requirements

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Effective requirements at `NewSchema`

**Files:**
- Modify: `object.go` (`fieldDef`), `schema.go` (`objectType`, `build()` phase 6), `authz_shape.go` (`validateAuthDirectives`, new `resolveAuthRequirements`)
- Test: `authz_inherit_test.go`

**Interfaces:**
- Consumes: `Requirement.And`, `maxRequirementGroups`, `groupCount` (Task 1); `requirementOf` (existing, `authz_shape.go`).
- Produces: `fieldDef.requires Requirement`; `objectType.requires Requirement`; `(*schemaBuilder).resolveAuthRequirements(s *Schema)`.

- [ ] **Step 1: Write the failing tests**

Append to `authz_inherit_test.go` (add `"strings"` to its imports):

```go
const inheritSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
interface Pet @requiresScopes(scopes: [["pet:read"]]) {
  name: String!
  secret: String! @requiresScopes(scopes: [["pet:secret"]])
}
type Dog implements Pet @requiresScopes(scopes: [["dog:read"]]) {
  name: String!
  secret: String!
  bark: String! @requiresScopes(scopes: [["dog:bark"]])
}
type Query { pet: Pet! }
`

func inheritSchema(t testing.TB) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(inheritSDL),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog",
			Field("name", func(*authzDog) string { return "rex" }),
			Field("secret", func(*authzDog) string { return "bones" }),
			Field("bark", func(*authzDog) string { return "woof" }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}

func TestEffectiveRequirementsAreComputedAtBuild(t *testing.T) {
	s := inheritSchema(t)
	dog := s.objects["Dog"]
	all := func(scopes ...string) map[string]bool {
		m := map[string]bool{}
		for _, sc := range scopes {
			m[sc] = true
		}
		return m
	}

	if dog.requires.Satisfied(all("dog:read")) {
		t.Error("Dog's type-level requirement ignores its interface's type-level requirement")
	}
	if !dog.requires.Satisfied(all("dog:read", "pet:read")) {
		t.Error("Dog's type-level requirement is not dog:read AND pet:read")
	}

	name := dog.fields["name"].requires
	if !name.Satisfied(all("dog:read", "pet:read")) || name.Satisfied(all("dog:read")) {
		t.Errorf("Dog.name should inherit exactly dog:read AND pet:read, got %v", name.anyOf)
	}

	secret := dog.fields["secret"].requires
	if secret.Satisfied(all("dog:read", "pet:read")) {
		t.Error("Dog.secret does not inherit Pet.secret's field-level requirement")
	}
	if !secret.Satisfied(all("dog:read", "pet:read", "pet:secret")) {
		t.Errorf("Dog.secret should need dog:read, pet:read and pet:secret, got %v", secret.anyOf)
	}

	bark := dog.fields["bark"].requires
	if !bark.Satisfied(all("dog:read", "pet:read", "dog:bark")) || bark.Satisfied(all("dog:read", "pet:read")) {
		t.Errorf("Dog.bark should add its own dog:bark, got %v", bark.anyOf)
	}

	if q := s.objects["Query"].fields["pet"].requires; !q.IsZero() {
		t.Errorf("Query.pet declares nothing and inherits nothing, got %v", q.anyOf)
	}
}

func TestEffectiveRequirementOverTheGroupCapFailsBuild(t *testing.T) {
	// 9 groups on the object times 9 on the field is 81, over the cap of 64.
	nine := func(prefix string) string {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < 9; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(`["` + prefix + string(rune('a'+i)) + `"]`)
		}
		b.WriteString("]")
		return b.String()
	}
	sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Big @requiresScopes(scopes: ` + nine("o") + `) { f: String! @requiresScopes(scopes: ` + nine("f") + `) }
type Query { big: Big! }
`
	_, err := NewSchema(SDL(sdl),
		Query(Field("big", func(Root) *authzFoo { return &authzFoo{} })),
		Object[authzFoo]("Big", Field("f", func(*authzFoo) string { return "" })),
	)
	if err == nil {
		t.Fatal("NewSchema accepted an effective requirement of 81 groups")
	}
	if !strings.Contains(err.Error(), "Big.f") {
		t.Errorf("error does not name the coordinate: %v", err)
	}
}

func TestRequiresScopesOnAnUnenforcedLocationFailsBuild(t *testing.T) {
	cases := []struct {
		name  string
		extra string // appended SDL carrying the misplaced directive
		coord string
	}{
		{"union", `union U @requiresScopes(scopes: [["x"]]) = Q2`, "U"},
		{"enum", `enum E @requiresScopes(scopes: [["x"]]) { A }`, "E"},
		{"enum value", `enum E2 { A @requiresScopes(scopes: [["x"]]) }`, "E2.A"},
		{"scalar", `scalar S @requiresScopes(scopes: [["x"]])`, "S"},
		{"input object", `input I @requiresScopes(scopes: [["x"]]) { a: String }`, "I"},
		{"input field", `input J { a: String @requiresScopes(scopes: [["x"]]) }`, "J.a"},
		{"argument", `type Q3 { f(a: String @requiresScopes(scopes: [["x"]])): String }`, "Q3.f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdl := `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE | UNION | ENUM | ENUM_VALUE | SCALAR | INPUT_OBJECT | INPUT_FIELD_DEFINITION | ARGUMENT_DEFINITION
type Q2 { a: String }
type Query { ok: String }
` + tc.extra
			_, err := NewSchema(SDL(sdl), Query(Field("ok", func(Root) string { return "" })))
			if err == nil {
				t.Fatalf("NewSchema accepted @requiresScopes on a %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.coord) || !strings.Contains(err.Error(), "not enforced") {
				t.Errorf("error should name %q and say it is not enforced: %v", tc.coord, err)
			}
		})
	}
}
```

These build schemas that also fail for unrelated reasons (unbound types such as `Q2`, `U`, `E`). That is fine: `NewSchema` joins every error, and each subtest asserts only that the joined error contains the placement error. Confirm by reading one failure message that the placement error is really in it rather than the test passing on an unrelated error — if the joined message does not contain `not enforced`, the assertion fails, so the `strings.Contains` check is what guards this.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestEffectiveRequirement|TestRequiresScopesOnAnUnenforcedLocation' .`
Expected: build failure (`dog.requires undefined`). After adding just the struct fields (Step 3's first two edits) the build succeeds and the tests fail on their assertions — run again at that point and record that output as the RED evidence.

- [ ] **Step 3: Implement**

In `object.go`, add to `fieldDef`:

```go
	// requires is the field's effective authorization requirement: its own
	// @requiresScopes AND its object type's AND every implemented
	// interface's type-level and same-named field requirement. It is
	// computed once in NewSchema so plan compile and RequireAuthCoverage
	// read the same value; the zero value means nothing is required.
	requires Requirement
```

In `schema.go`, add to `objectType`:

```go
	// requires is the object's effective type-level requirement (its own
	// AND its interfaces'), which guards __typename on this type.
	requires Requirement
```

In `schema.go` `build()`, phase 6 becomes:

```go
	// Phase 6: coverage.
	b.validateCoverage(s)
	b.validateAuthDirectives()
	b.resolveAuthRequirements(s)
	b.validateAuthCoverage(s)
```

In `authz_shape.go`, replace `validateAuthDirectives` with:

```go
// validateAuthDirectives rejects a malformed or misplaced @requiresScopes at
// schema build, joined into NewSchema's errors like every other Go-vs-SDL
// shape mismatch. gqlparser checks the directive's name, location and
// argument presence but never the "scopes" value's shape: a flat list of
// strings decodes to a requirement satisfied by everyone while still creating
// a site. And a placement the engine does not enforce -- a union, enum, enum
// value, scalar, input object, input field or argument -- would read as
// guarded while guarding nothing, so it is an error rather than a no-op.
func (b *schemaBuilder) validateAuthDirectives() {
	for name, def := range b.ast.Types {
		if def.BuiltIn {
			continue
		}
		switch def.Kind {
		case ast.Object, ast.Interface:
			b.checkRequiresScopes(name, def.Directives)
			for _, f := range def.Fields {
				coord := coordinate(name, f.Name)
				b.checkRequiresScopes(coord, f.Directives)
				for _, a := range f.Arguments {
					b.rejectUnenforced(coord, "an argument", a.Directives)
				}
			}
		case ast.InputObject:
			b.rejectUnenforced(name, "an input object", def.Directives)
			for _, f := range def.Fields {
				b.rejectUnenforced(coordinate(name, f.Name), "an input field", f.Directives)
			}
		case ast.Enum:
			b.rejectUnenforced(name, "an enum", def.Directives)
			for _, v := range def.EnumValues {
				b.rejectUnenforced(coordinate(name, v.Name), "an enum value", v.Directives)
			}
		case ast.Union:
			b.rejectUnenforced(name, "a union", def.Directives)
		case ast.Scalar:
			b.rejectUnenforced(name, "a scalar", def.Directives)
		}
	}
}

func (b *schemaBuilder) rejectUnenforced(coord, what string, ds ast.DirectiveList) {
	if ds.ForName(authDirective) != nil {
		b.errorf("%s: @%s is not enforced on %s; declare it on an object, interface or field", coord, authDirective, what)
	}
}

// resolveAuthRequirements stores each bound field's effective requirement on
// its fieldDef and each object's type-level one on its objectType. Every input
// is schema-level and immutable after build, so the value is identical on
// every path that reaches a field -- which is what keeps a memoized selection
// set shared between parents safe to index once.
func (b *schemaBuilder) resolveAuthRequirements(s *Schema) {
	for name, obj := range s.objects {
		typeReq, _ := requirementOf(obj.def.Directives)
		for _, iname := range obj.def.Interfaces {
			if idef := b.ast.Types[iname]; idef != nil {
				r, _ := requirementOf(idef.Directives)
				typeReq = typeReq.And(r)
			}
		}
		if typeReq.groupCount() > maxRequirementGroups {
			b.errorf("%s: effective @%s has %d groups, more than %d", name, authDirective, typeReq.groupCount(), maxRequirementGroups)
		}
		obj.requires = typeReq

		for _, fd := range obj.fields {
			req, _ := requirementOf(fd.def.Directives)
			req = req.And(typeReq)
			for _, iname := range obj.def.Interfaces {
				idef := b.ast.Types[iname]
				if idef == nil {
					continue
				}
				if ifd := idef.Fields.ForName(fd.name); ifd != nil {
					r, _ := requirementOf(ifd.Directives)
					req = req.And(r)
				}
			}
			if req.groupCount() > maxRequirementGroups {
				b.errorf("%s: effective @%s has %d groups, more than %d", coordinate(name, fd.name), authDirective, req.groupCount(), maxRequirementGroups)
			}
			fd.requires = req
		}
	}
}
```

Confirm by reading `object.go` that `obj.fields` is keyed by field name and `fd.name` is the SDL field name, and that `obj.def.Interfaces` is `[]string` on `*ast.Definition`. If `s.objects` also holds introspection types (`__Type`, …), they carry no directives and resolve to the zero requirement — leave them.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestEffectiveRequirement|TestRequiresScopesOnAnUnenforcedLocation|TestNewSchemaRejectsMalformedScopes' .`
Expected: PASS.

- [ ] **Step 5: Prove the tests can fail**

(a) In `resolveAuthRequirements`, delete the inner interface-field loop. Run `TestEffectiveRequirementsAreComputedAtBuild`; confirm it FAILS on `Dog.secret`. Revert.
(b) Delete the `ast.Union` case. Run the unenforced-location test; confirm the `union` subtest FAILS. Revert.

- [ ] **Step 6: Run the full root package**

Run: `go test -race -count=1 .`
Expected: PASS. Nothing reads `requires` yet, so every existing test is unaffected.

- [ ] **Step 7: Commit**

```bash
git add object.go schema.go authz_shape.go authz_inherit_test.go
git commit -m "feat: compute effective authorization requirements at NewSchema

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Enforce inherited requirements and guard `__typename`

**Files:**
- Modify: `authz_shape.go` (`buildAuthShape`, `shapeBuilder`), `plan.go` (`compilePlan`), `exec_object.go` (`writeField`, `writeFieldValue`), `authz.go` (`validFor`)
- Test: `authz_inherit_test.go`

**Interfaces:**
- Consumes: `fieldDef.requires`, `objectType.requires` (Task 2); `ScopeAuthorizer`, `WithAuthorizer`, `AuthorizerFunc`, `Decision.Set`, `Null`, `Deny`, `CodeForbidden`, `run`, `planForTest` (existing).
- Produces: `buildAuthShape(root *objectType, sel *selectionSet) *AuthShape`; `SiteObject` sites with `Coord` = object name, `Field` nil, `Requires` = `objectType.requires`.

- [ ] **Step 1: Write the failing tests**

Append to `authz_inherit_test.go` (add `"context"` to imports):

```go
func inheritExec(t testing.TB, held ...string) *Executor {
	t.Helper()
	have := map[string]bool{}
	for _, h := range held {
		have[h] = true
	}
	return NewExecutor(inheritSchema(t), WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return have })))
}

func TestInheritedRequirementDeniesField(t *testing.T) {
	cases := []struct {
		name  string
		held  []string
		query string
		deny  bool
	}{
		{"object-level denies a field that declares nothing", []string{"pet:read"}, `{ pet { name } }`, true},
		{"interface type-level denies", []string{"dog:read"}, `{ pet { name } }`, true},
		{"interface field-level denies the implementer's field", []string{"dog:read", "pet:read"}, `{ pet { secret } }`, true},
		{"same, selected through an inline fragment", []string{"dog:read", "pet:read"}, `{ pet { ... on Dog { secret } } }`, true},
		{"everything held allows", []string{"dog:read", "pet:read", "pet:secret"}, `{ pet { name secret } }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, inheritExec(t, tc.held...), tc.query, "")
			if !tc.deny {
				if len(resp.Errors) > 0 {
					t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
				}
				return
			}
			if len(resp.Errors) == 0 {
				t.Fatalf("not denied; data = %s", resp.Data)
			}
			if got := resp.Errors[0].Extensions["code"]; got != CodeForbidden {
				t.Errorf("code = %v, want %v", got, CodeForbidden)
			}
		})
	}
}

// Apollo clients add __typename to every selection. Unguarded, it counts the
// rows of a guarded type and confirms a given one exists.
func TestTypenameIsGuardedByTheObjectRequirement(t *testing.T) {
	resp := run(t, inheritExec(t), `{ pet { __typename } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatalf("__typename of a guarded type was not denied; data = %s", resp.Data)
	}
	if string(resp.Data) != "null" && resp.Data != nil {
		t.Errorf("__typename is String!, so its denial must bubble; data = %s", resp.Data)
	}

	resp = run(t, inheritExec(t, "dog:read", "pet:read"), `{ pet { __typename } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("held scopes still denied __typename: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"pet":{"__typename":"Dog"}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

func TestTypenameOfAnUnguardedTypeHasNoSite(t *testing.T) {
	e := NewExecutor(shapeSchema(t))
	p, _, perrs := planForTest(t, e, `{ me { __typename id } }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	for _, s := range p.shape.Sites() {
		if s.Kind == SiteObject {
			t.Errorf("unguarded type User produced an object site %q", s.Coord)
		}
	}
}

func TestObjectSiteAdmitsOnlyAllowAndDeny(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Outcome
	}{
		{"Null", Null()},
		{"Zero", Zero()},
		{"Redact", Redact(func(v any) any { return v })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var setErr error
			e := NewExecutor(inheritSchema(t), WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					for i, s := range shape.Sites() {
						if s.Kind == SiteObject {
							setErr = d.Set(i, tc.o)
						}
					}
					return nil
				})))
			run(t, e, `{ pet { __typename } }`, "")
			if setErr == nil {
				t.Errorf("Decision.Set accepted %s on an object site", tc.name)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestInheritedRequirementDeniesField|TestTypename|TestObjectSiteAdmitsOnlyAllowAndDeny' .`
Expected: the deny cases, the `__typename` test and the object-site test FAIL (nothing inherits, no object site exists). `TestTypenameOfAnUnguardedTypeHasNoSite` passes already — it is a regression pin for the next step. Record the failures.

- [ ] **Step 3: Build sites from the stored requirements**

In `plan.go` `compilePlan`: `p.shape = buildAuthShape(p.root, p.sel)`.

In `authz_shape.go`, replace `buildAuthShape`, `shapeBuilder`, `walk` and `field` with:

```go
func buildAuthShape(root *objectType, sel *selectionSet) *AuthShape {
	b := &shapeBuilder{}
	b.walk(root, nil, sel)
	if len(b.sites) == 0 {
		return nil
	}
	slices.Sort(b.scopes)
	return &AuthShape{sites: b.sites, scopes: slices.Compact(b.scopes)}
}

type shapeBuilder struct {
	sites      []AuthSite
	scopes     []string
	seen       map[*selectionSet]bool
	objectSite map[*objectType]int32
}

// walk visits a selection set whose parent is obj (concrete) or abs (abstract).
// compileSelection keys its memo by that parent, so a set -- and any
// __typename inside it -- belongs to exactly one parent even when several
// fields reach it, which is what makes an object site well defined.
func (b *shapeBuilder) walk(obj *objectType, abs *abstractType, sel *selectionSet) {
	if sel == nil {
		return
	}
	// keep the existing comment explaining why the seen set is load-bearing
	if b.seen == nil {
		b.seen = make(map[*selectionSet]bool)
	}
	if b.seen[sel] {
		return
	}
	b.seen[sel] = true

	for _, f := range sel.fields {
		b.field(obj, f)
	}
	if abs != nil && len(sel.byType) > 0 {
		names := make([]string, 0, len(sel.byType))
		for name := range sel.byType {
			names = append(names, name)
		}
		// Sorted so a document's site indices do not depend on map order.
		slices.Sort(names)
		for _, name := range names {
			b.walk(abs.possible[name], nil, sel.byType[name])
		}
	}
}

func (b *shapeBuilder) field(obj *objectType, f *planField) {
	f.authIdx = -1
	switch {
	case f.kind == fieldTypename:
		if obj != nil && !obj.requires.IsZero() {
			f.authIdx = b.objectSiteFor(obj)
		}
	case f.def != nil && !f.def.requires.IsZero():
		f.authIdx = int32(len(b.sites))
		b.sites = append(b.sites, AuthSite{
			Coord:    coordinate(f.def.object.name, f.name),
			Field:    f.def.def,
			Object:   f.def.object.def,
			Kind:     SiteOutput,
			Requires: f.def.requires,
			leaf:     f.def.leaf,
		})
		b.scopes = append(b.scopes, f.def.requires.Scopes()...)
	}
	b.walk(f.target, f.abstract, f.sub)
}

// objectSiteFor returns the one SiteObject site for obj in this plan, so every
// __typename on a type shares a decision.
func (b *shapeBuilder) objectSiteFor(obj *objectType) int32 {
	if i, ok := b.objectSite[obj]; ok {
		return i
	}
	if b.objectSite == nil {
		b.objectSite = make(map[*objectType]int32)
	}
	i := int32(len(b.sites))
	b.sites = append(b.sites, AuthSite{
		Coord:    obj.name,
		Object:   obj.def,
		Kind:     SiteObject,
		Requires: obj.requires,
	})
	b.scopes = append(b.scopes, obj.requires.Scopes()...)
	b.objectSite[obj] = i
	return i
}
```

Keep the existing explanatory comment on the `seen` set (replace the placeholder line above with it verbatim). Confirm against `plan.go` that `planField.target` is set for a concrete composite field and `planField.abstract` for an abstract one, and that an abstract `selectionSet` carries only `byType`.

- [ ] **Step 4: Admit only Allow and Deny on an object site**

At the top of `validFor` in `authz.go`, before the `switch`:

```go
	// An object site guards __typename, a String! with no resolver: Null
	// would write a silent spec-violating null, and Zero and Redact have no
	// field to act on.
	if site.Kind == SiteObject && o.act != actionAllow && o.act != actionDeny {
		return Errorf("authorization: only Allow and Deny are valid for %s, an object site guarding __typename", site.Coord)
	}
```

- [ ] **Step 5: Route `__typename` through the auth check**

In `exec_object.go` `writeFieldValue`, move the `fieldTypename` branch to immediately after the auth block:

```go
	// -1 on a field that declares nothing, so the ordinary path pays one
	// compare on a struct already in cache.
	if st.decision != nil && f.authIdx >= 0 {
		if done, ok := st.enforceAuth(ctx, w, f, path); done {
			return ok
		}
	}
	if f.kind == fieldTypename {
		w.String(obj.name)
		return true
	}
```

In `writeField`, a failed `__typename` must bubble like any non-null field — it has no `fieldDef`, so today it would be rewritten as `null` into a `String!` position:

```go
	if f.kind == fieldTypename || (f.def != nil && f.def.typ.NonNull) {
		return false
	}
```

Check `st.fieldError` and `enforceAuth`'s Deny path for any dereference of `f.def` — `__typename` has none. If one exists, guard it and say so in the report.

- [ ] **Step 6: Run to verify they pass**

Run: `go test -race -run 'TestInheritedRequirementDeniesField|TestTypename|TestObjectSiteAdmitsOnlyAllowAndDeny|TestAuthShape|TestOutcome|TestSubscri' .`
Expected: PASS.

- [ ] **Step 7: Prove the tests can fail**

(a) In `shapeBuilder.field`, replace `f.def.requires` with `requirementOf(f.def.def.Directives)` (the pre-2a reading). Run `TestInheritedRequirementDeniesField`; the inherited cases must FAIL. Revert.
(b) Move the `__typename` branch back above the auth check. Run `TestTypenameIsGuardedByTheObjectRequirement`; it must FAIL. Revert.
(c) Revert only the `writeField` change. Run the same test; the bubbling assertion must FAIL. Revert.

- [ ] **Step 8: Run the full root package**

Run: `go test -race -count=1 .`
Expected: PASS. That includes `TestRequireAuthCoverageRejectsObjectLevelRequirement` and `TestRequireAuthCoverageRejectsInterfaceOnlyRequirement`: coverage does not change until Task 4, so between the two tasks it stays stricter than enforcement (fail-closed), never looser.

- [ ] **Step 9: Commit**

```bash
git add authz.go authz_shape.go plan.go exec_object.go authz_inherit_test.go
git commit -m "feat: enforce inherited requirements and guard __typename

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Coverage reads the effective requirement

**Files:**
- Modify: `authz_shape.go` (`validateAuthCoverage`)
- Modify: `authz_test.go` (the two tests that flip)
- Test: `authz_inherit_test.go`

**Interfaces:**
- Consumes: `fieldDef.requires` (Task 2).

- [ ] **Step 1: Flip the two tests that encoded the old limit**

In `authz_test.go`, replace `TestRequireAuthCoverageRejectsObjectLevelRequirement` (and its preceding comment) with:

```go
// An object-level @requiresScopes is inherited by every field of the object
// and enforced (resolveAuthRequirements, shapeBuilder.field), so it covers
// them.
func TestRequireAuthCoverageAcceptsObjectLevelRequirement(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
directive @public on FIELD_DEFINITION | OBJECT
type Foo @requiresScopes(scopes: [["x"]]) { secret: String! }
type Query { foo: Foo! @public }
`
	if _, err := NewSchema(SDL(sdl),
		Query(Field("foo", func(Root) *authzFoo { return &authzFoo{} })),
		Object[authzFoo]("Foo", Field("secret", func(*authzFoo) string { return "" })),
		RequireAuthCoverage(),
	); err != nil {
		t.Fatalf("NewSchema rejected a field covered by its object's enforced requirement: %v", err)
	}
}
```

Replace `TestRequireAuthCoverageRejectsInterfaceOnlyRequirement` (and its preceding comment) with:

```go
// A requirement on an interface's field definition is inherited by the
// implementer's same-named field and enforced, so it covers that field.
func TestRequireAuthCoverageAcceptsInterfaceFieldRequirement(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
directive @public on FIELD_DEFINITION | OBJECT
interface Pet { name: String! @public secret: String! @requiresScopes(scopes: [["x"]]) }
type Dog implements Pet { name: String! @public secret: String! }
type Query { pet: Pet! @public }
`
	if _, err := NewSchema(SDL(sdl),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog",
			Field("name", func(*authzDog) string { return "" }),
			Field("secret", func(*authzDog) string { return "" }),
		),
		RequireAuthCoverage(),
	); err != nil {
		t.Fatalf("NewSchema rejected Dog.secret, covered by Pet.secret's enforced requirement: %v", err)
	}
}
```

Leave the `authzFoo`, `authzPet`, `authzDog` type declarations in place — other tests use them.

- [ ] **Step 2: Add the negative pins**

Append to `authz_inherit_test.go`:

```go
// Exemption stays explicit per type: an interface's @public must not quietly
// exempt every implementer.
func TestRequireAuthCoverageInterfacePublicDoesNotExempt(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
directive @public on FIELD_DEFINITION | OBJECT | INTERFACE
interface Pet @public { name: String! }
type Dog implements Pet { name: String! }
type Query { pet: Pet! @public }
`
	_, err := NewSchema(SDL(sdl),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog", Field("name", func(*authzDog) string { return "" })),
		RequireAuthCoverage(),
	)
	if err == nil || !strings.Contains(err.Error(), "Dog.name") {
		t.Fatalf("interface @public exempted Dog.name; err = %v", err)
	}
}

// The requirement coverage accepts must be the one authorization enforces.
func TestRequireAuthCoverageAcceptsInterfaceTypeRequirement(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
directive @public on FIELD_DEFINITION | OBJECT
interface Pet @requiresScopes(scopes: [["x"]]) { name: String! }
type Dog implements Pet { name: String! }
type Query { pet: Pet! @public }
`
	if _, err := NewSchema(SDL(sdl),
		Query(Field("pet", func(Root) authzPet { return &authzDog{} })),
		Interface[authzPet]("Pet"),
		Object[authzDog]("Dog", Field("name", func(*authzDog) string { return "" })),
		RequireAuthCoverage(),
	); err != nil {
		t.Fatalf("NewSchema rejected a field covered by its interface's type-level requirement: %v", err)
	}
}
```

- [ ] **Step 3: Run to verify the right ones fail**

Run: `go test -race -run 'AuthCoverage' .`
Expected: the two flipped tests and `TestRequireAuthCoverageAcceptsInterfaceTypeRequirement` FAIL; `TestRequireAuthCoverageInterfacePublicDoesNotExempt` and the pre-existing coverage tests PASS. Record.

- [ ] **Step 4: Implement**

Replace `validateAuthCoverage` in `authz_shape.go`:

```go
// validateAuthCoverage fails the build for any bound field that has no
// effective requirement and no @public, when RequireAuthCoverage is on. It
// reads fieldDef.requires -- the same value shapeBuilder.field enforces -- so a
// field is covered exactly when authorization actually guards it. Exemption
// stays explicit: @public on the field or on its own object type, never
// inherited from an interface.
func (b *schemaBuilder) validateAuthCoverage(s *Schema) {
	if !b.authCoverage {
		return
	}
	for name, obj := range s.objects {
		if strings.HasPrefix(name, "__") {
			continue
		}
		if obj.def.Directives.ForName("public") != nil {
			continue
		}
		for _, fd := range obj.fields {
			if strings.HasPrefix(fd.name, "__") {
				continue
			}
			if fd.def.Directives.ForName("public") != nil {
				continue
			}
			if !fd.requires.IsZero() {
				continue
			}
			b.errorf("field %s declares no authorization; add @requiresScopes or @public", coordinate(obj.name, fd.name))
		}
	}
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test -race -run 'AuthCoverage|ComposedIntrospection' .`
Expected: PASS.

- [ ] **Step 6: Prove the coverage check still bites**

Make `validateAuthCoverage` treat every field as covered (`continue` unconditionally). Confirm `TestRequireAuthCoverageRejectsAnUndeclaredField` and `TestRequireAuthCoverageInterfacePublicDoesNotExempt` FAIL. Revert.

- [ ] **Step 7: Commit**

```bash
git add authz_shape.go authz_test.go authz_inherit_test.go
git commit -m "feat: count inherited requirements as authorization coverage

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Authorizer failures are not shown to clients

**Files:**
- Modify: `exec.go` (`toError` → `authorizerError`, `authorize` doc comment), `subscription.go` (call site)
- Test: `authz_inherit_test.go`

**Interfaces:**
- Consumes: `panicError` (exec.go), `CodeInternal`, `WithErrorPresenter`, `newAuthSubGuardedExecutor` (authz_test.go).
- Produces: `func authorizerError(ctx context.Context, err error) *Error`.

- [ ] **Step 1: Write the failing tests**

Append to `authz_inherit_test.go` (add `"errors"` to imports):

```go
var errPolicyDown = errors.New("dial tcp 10.0.3.7:8181: connect: connection refused")

func TestAuthorizerTransportErrorIsNotShownToClients(t *testing.T) {
	var presented error
	e := NewExecutor(shapeSchema(t),
		WithAuthorizer(AuthorizerFunc(func(context.Context, *AuthShape, *Decision) error {
			return errPolicyDown
		})),
		WithErrorPresenter(func(ctx context.Context, err error) *Error {
			presented = err
			return DefaultErrorPresenter(ctx, err)
		}),
	)
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("operation was not rejected")
	}
	if strings.Contains(resp.Errors[0].Message, "10.0.3.7") || strings.Contains(resp.Errors[0].Message, "dial tcp") {
		t.Errorf("client saw the policy backend's error: %q", resp.Errors[0].Message)
	}
	if got := resp.Errors[0].Extensions["code"]; got != CodeInternal {
		t.Errorf("code = %v, want %v", got, CodeInternal)
	}
	var ge *Error
	if !errors.As(presented, &ge) || !errors.Is(ge.Err, errPolicyDown) {
		t.Errorf("the presenter lost the original cause; got %v", presented)
	}
}

func TestAuthorizerStructuredErrorStillShown(t *testing.T) {
	e := NewExecutor(shapeSchema(t), WithAuthorizer(AuthorizerFunc(
		func(context.Context, *AuthShape, *Decision) error {
			return Errorf("tenant suspended").WithCode(CodeForbidden)
		})))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 || resp.Errors[0].Message != "tenant suspended" {
		t.Fatalf("an *Error built on purpose was not passed through: %s", errorsJSON(resp.Errors))
	}
}

func TestAuthorizerTransportErrorIsNotShownAtSubscribe(t *testing.T) {
	src, e := newAuthSubGuardedExecutor(t, nil)
	e.authorizer = AuthorizerFunc(func(context.Context, *AuthShape, *Decision) error {
		return errPolicyDown
	})
	_, err := e.Subscribe(t.Context(), &Request{Query: `subscription { messages { id } }`})
	var se *SubscribeError
	if !errors.As(err, &se) || se.Response == nil || len(se.Response.Errors) == 0 {
		t.Fatalf("Subscribe error = %v, want a SubscribeError with a response", err)
	}
	if msg := se.Response.Errors[0].Message; strings.Contains(msg, "10.0.3.7") {
		t.Errorf("subscriber saw the policy backend's error: %q", msg)
	}
	if n := src.opens.Load(); n != 0 {
		t.Errorf("source opened %d times", n)
	}
}
```

Read `newAuthSubGuardedExecutor` in `authz_test.go` first: if its source type has no `opens` counter, or if setting `e.authorizer` after construction is not how that helper is meant to be used, build an equivalent executor with `WithAuthorizer` instead and keep the three assertions.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestAuthorizerTransportError|TestAuthorizerStructuredError' .`
Expected: the two transport-error tests FAIL (message contains `dial tcp`); the structured test passes (regression pin). Record.

- [ ] **Step 3: Implement**

In `exec.go`, replace `toError` with:

```go
// authorizerError prepares an Authorize failure for presentation. An *Error
// the Authorizer built on purpose passes through. Anything else is typically a
// policy backend's own failure -- a transport error carrying an internal
// address -- and would otherwise reach the client verbatim, both disclosing
// the address and making an outage look like a denial. It is presented as a
// generic internal error with the original kept as the cause, and logged
// unless it is a recovered panic, which runAuthorizer has already logged.
func authorizerError(ctx context.Context, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var p *panicError
	if !errors.As(err, &p) {
		slog.ErrorContext(ctx, "graphql: authorizer failed", "error", err)
	}
	return (&Error{Message: "internal system error", Err: err}).WithCode(CodeInternal)
}
```

Update both call sites (`exec.go` in `runOperation`, `subscription.go` in the `open` handler) from `toError(err)` to `authorizerError(ctx, err)`. Grep for any other `toError(` use; there should be none.

Update the doc comment on `authorize` to say a non-nil error rejects the whole operation, one subscription event, or — when called as the stream opens — the subscription itself.

Confirm `(*Error).WithCode` returns `*Error` and that `DefaultErrorPresenter` serialises `Message` rather than `Err`'s text; if it prefers `Err`, say so in the report rather than working around it.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestAuthorizer|TestSubscribe' .`
Expected: PASS, including the pre-existing panic tests.

- [ ] **Step 5: Prove the test can fail**

Make `authorizerError` return `&Error{Message: err.Error(), Err: err}` for the non-`*Error` case. Confirm `TestAuthorizerTransportErrorIsNotShownToClients` FAILS. Revert.

- [ ] **Step 6: Commit**

```bash
git add exec.go subscription.go authz_inherit_test.go
git commit -m "fix: stop showing Authorizer transport failures to clients

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Performance gate

`__typename` now passes through the auth check, and `writeField`'s failure branch gained a compare. Both are on the request path.

**Files:**
- Create: `authz_typename_bench_test.go`

- [ ] **Step 1: Add the benchmark**

The controller has already built `before.exe` from the pre-2a base with this exact file added, so it must be byte-identical:

```go
package graphql

import (
	"context"
	"testing"
)

// BenchmarkExecuteTypenameHeavy measures __typename-dense responses, the shape
// Apollo clients produce, with no authorizer registered.
func BenchmarkExecuteTypenameHeavy(b *testing.B) {
	_, e := newFixtureExecutor(b)
	ctx := context.Background()
	req := &Request{Query: `{ users { __typename id name friends { __typename id } } }`}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Execute(ctx, req).Release()
	}
}
```

- [ ] **Step 2: Interleave against the base**

`before.exe` is at `.superpowers/sdd/2026-09-17-authz-2a-inheritance/before.exe`. Do not create worktrees.

```bash
WS=.superpowers/sdd/2026-09-17-authz-2a-inheritance
BENCH='BenchmarkExecuteUsers|BenchmarkExecuteConcurrentList|BenchmarkExecuteTypenameHeavy'
go test -c -o "$WS/after.exe" .
rm -f "$WS/before.txt" "$WS/after.txt"
for i in $(seq 1 12); do
  "$WS/before.exe" -test.run xxx -test.bench "$BENCH" -test.benchmem -test.count=1 >> "$WS/before.txt"
  "$WS/after.exe"  -test.run xxx -test.bench "$BENCH" -test.benchmem -test.count=1 >> "$WS/after.txt"
done
benchstat "$WS/before.txt" "$WS/after.txt"
```

Sequential runs are not acceptable on this machine (see CLAUDE.md, "Two sequential runs measure the machine as much as the change").

**Acceptance:** no statistically significant regression on any of the three; allocs/op equal. Run `go test -run TestStructSizes -v .` and confirm `execState` 64 and `OperationContext` 160.

**If it regresses:** report the table and your diagnosis; do not tune.

- [ ] **Step 3: Commit**

```bash
git add authz_typename_bench_test.go
git commit -m "test: benchmark __typename-dense responses

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Documentation

**Files:**
- Modify: `doc.go` (the paragraph beginning "RequireAuthCoverage is an opt-in"), `CLAUDE.md` (the "Authorization is compiled, not wrapped." paragraph)

- [ ] **Step 1: `doc.go`**

Rewrite that paragraph to state what is now true: a field's effective requirement is its own `@requiresScopes` AND its object type's AND its interfaces' type-level and same-named field requirements, computed at `NewSchema`; `__typename` is guarded by its object's type-level requirement through a `SiteObject`, on which only `Allow` and `Deny` are valid; `@requiresScopes` anywhere else is a build error; `RequireAuthCoverage` counts a field as covered exactly when authorization guards it, and `@public` exempts only on the field or its own object type. Keep the sentence that `SiteFilterArg` and `SiteInputWrite` are not yet populated. Add one sentence that a non-`*Error` returned by `Authorize` is presented as a generic internal error.

- [ ] **Step 2: `CLAUDE.md`**

In the "Authorization is compiled, not wrapped." paragraph, replace the sentence block beginning "**Object-level and interface-level `@requiresScopes` are not enforced**" with: effective requirements are resolved once in `resolveAuthRequirements` and stored on `fieldDef.requires` / `objectType.requires`; `shapeBuilder` and `validateAuthCoverage` both read those, so **never compute a requirement from `requirementOf` directly at plan compile or in coverage** — that is how enforcement and coverage would silently diverge. Touch nothing else in `CLAUDE.md`.

- [ ] **Step 3: Verify every claim**

For each sentence changed, cite the code line that makes it true in the report.

- [ ] **Step 4: Gate and commit**

Run: `go vet ./... && sh scripts/gate.sh -short`

```bash
git add doc.go CLAUDE.md
git commit -m "docs: document inherited authorization requirements

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review Notes

- **Spec §9.2 coverage:** inheritance (Tasks 2, 3), group cap (Task 2), `__typename` + `SiteObject` Allow/Deny only (Task 3), unenforced placements are build errors (Task 2), coverage reads the stored value and interface `@public` does not exempt (Task 4), Authorizer error hardening (Task 5). §9.1 needs no code. §9.3 is Plan 2b.
- **Found while planning, beyond §9.2:** `writeField` rewrote a failed `__typename` as `null` because it has no `fieldDef` (Task 3 Step 5); `validFor`'s Null rule skipped `Field == nil` sites (Task 3 Step 4 supersedes it for `SiteObject`).
- **Type consistency:** `fieldDef.requires`, `objectType.requires`, `Requirement.And`, `groupCount`, `maxRequirementGroups`, `buildAuthShape(root, sel)`, `authorizerError(ctx, err)` are used identically across tasks.
- **Flagged for the implementer to verify rather than assumed:** `planField.target`/`abstract` semantics; `st.fieldError` and `enforceAuth` never dereferencing `f.def` for `__typename`; `newAuthSubGuardedExecutor`'s shape; `DefaultErrorPresenter` serialising `Message`.
