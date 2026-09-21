# Pluggable Requirement Directives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a schema declare which SDL directives the engine reads into a `Requirement`, so a legacy spelling like `@auth(requires: [String!])` is enforced without renaming 1,589 sites, and without weakening any check that applies to `@requiresScopes` today.

**Architecture:** `schemaBuilder` gains a list of declared directive shapes, defaulted to the Apollo one. The four places welded to the spelling — reading, literal type-checking, misplacement rejection, error wording — all iterate that list instead of a constant. `resolveAuthRequirements` keeps its position as the one place a requirement is computed.

**Tech Stack:** Go 1.27, `gqlparser/v2` AST only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-21-pluggable-requirement-directives-design.md`

## Global Constraints

- Root package may depend only on `gqlparser/v2` and the standard library.
- No reflection added to the request path. This work is `NewSchema`-time only.
- `execState` 64 bytes, `OperationContext` 160, `planField` 176 — `TestStructSizes` must keep passing untouched.
- Every change is TDD: write the failing test, watch it fail for the right reason, then implement.
- Gate before each commit: `go vet ./... && go test -race -count=1 .`
- Full gate before the final commit: `sh scripts/gate.sh` and `golangci-lint run ./...`.
- Comments explain why, not what. English only.
- Commit messages: imperative, lower-case type prefix. End with the Co-Authored-By line used elsewhere in this repository's history.

## File Structure

| File | Responsibility |
|---|---|
| `authz.go` | Modify: add `ScopeShape`, its constants, `reqDirective`, and the `RequirementDirective` SchemaOption. |
| `schema.go` | Modify: add `reqDirectives []reqDirective` to `schemaBuilder`; seed the default before options apply. |
| `authz_shape.go` | Modify: `requirementOf` becomes a method over the list; literal check, misplacement rejection and cap wording take the directive name. |
| `authz_directive_test.go` | Create: every test in this plan. |

---

### Task 1: Declare the surface and reject a bad declaration

**Files:**
- Modify: `authz.go` (append near `NewRequirement`)
- Modify: `schema.go` (`schemaBuilder` struct and its construction)
- Modify: `authz_shape.go` (new validator, called from `build()`)
- Test: `authz_directive_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: `type ScopeShape uint8`; constants `ScopesNested`, `ScopesAllOf`, `ScopesAnyOf`; `func RequirementDirective(name, arg string, shape ScopeShape) SchemaOption`; unexported `type reqDirective struct{ name, arg string; shape ScopeShape }`; field `schemaBuilder.reqDirectives []reqDirective`; `func (b *schemaBuilder) validateRequirementDirectives()`.

- [ ] **Step 1: Write the failing test**

Create `authz_directive_test.go`:

```go
package graphql

import (
	"strings"
	"testing"
)

const legacySDL = `
directive @auth(requires: [String!]) on OBJECT | FIELD_DEFINITION
type Query { secret: String! @auth(requires: ["admin"]) }
`

// A bad declaration must fail the build. Each of these is a way to configure a
// directive that would otherwise read as "no requirement" on every site while
// the schema builds clean.
func TestRequirementDirectiveDeclarationErrors(t *testing.T) {
	cases := []struct {
		name    string
		opt     SchemaOption
		wantErr string
	}{
		{"undeclared shape", RequirementDirective("auth", "requires", 0), "shape must be declared"},
		{"shape out of range", RequirementDirective("auth", "requires", ScopeShape(99)), "shape must be declared"},
		{"directive not in the SDL", RequirementDirective("nosuch", "requires", ScopesAllOf), "no directive @nosuch"},
		{"argument the directive lacks", RequirementDirective("auth", "scope", ScopesAllOf), `has no argument "scope"`},
		{"redeclaring the built-in", RequirementDirective("requiresScopes", "scopes", ScopesAllOf), "already declared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(legacySDL), c.opt,
				Query(Field("secret", func(Root) string { return "s" })))
			if err == nil {
				t.Fatal("declaration was accepted")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// Declaring one custom name twice is the same hazard as redeclaring the
// built-in: two option calls silently racing to be last.
func TestRequirementDirectiveRejectsADuplicateName(t *testing.T) {
	_, err := NewSchema(SDL(legacySDL),
		RequirementDirective("auth", "requires", ScopesAllOf),
		RequirementDirective("auth", "requires", ScopesAnyOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil || !strings.Contains(err.Error(), "already declared") {
		t.Fatalf("error = %v, want a duplicate-name rejection", err)
	}
}

// The control: without it, every test above could pass for the wrong reason if
// NewSchema rejected the fixture outright.
func TestRequirementDirectiveValidDeclarationBuilds(t *testing.T) {
	if _, err := NewSchema(SDL(legacySDL),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" }))); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail for the right reason**

Run: `go test -count=1 -run TestRequirementDirective . 2>&1 | head -20`

Expected: build failure — `undefined: RequirementDirective`, `undefined: ScopesAllOf`. If any test *passes*, stop: the names already exist and this plan is stale.

- [ ] **Step 3: Add the type and the option**

In `authz.go`, after `NewRequirement`:

```go
// ScopeShape says how a requirement directive's argument composes into a
// Requirement. There is no valid zero value: reading a flat [String!] as AND
// when the author meant OR silently widens access, and the SDL cannot tell the
// two apart -- @auth(requires: ["a","b"]) is the same text either way.
type ScopeShape uint8

const (
	// ScopesNested reads [[String!]!] as an OR of ANDs, which is Apollo's
	// @requiresScopes.
	ScopesNested ScopeShape = iota + 1
	// ScopesAllOf reads [String!] as one AND group: every scope is required.
	ScopesAllOf
	// ScopesAnyOf reads [String!] as one group per scope: any one suffices.
	ScopesAnyOf
)

// reqDirective is one declared spelling. The engine holds the name so it can
// reject a misplacement and name the offender in an error; a caller-supplied
// function could not be rejected that way, which is why this is a value and
// not a hook.
type reqDirective struct {
	name  string
	arg   string
	shape ScopeShape
}

// RequirementDirective declares an additional SDL directive read into a
// Requirement, so a schema that spells its requirements @auth(requires:) is
// enforced without renaming every site. @requiresScopes stays active, so one
// schema can carry a legacy spelling for existing coordinates and the Apollo
// one for new work; a coordinate carrying both must satisfy both.
func RequirementDirective(name, arg string, shape ScopeShape) SchemaOption {
	return schemaOptionFunc(func(b *schemaBuilder) {
		b.reqDirectives = append(b.reqDirectives, reqDirective{name: name, arg: arg, shape: shape})
	})
}
```

`schemaOptionFunc` is the existing adapter (`schema.go:19`); `RequireAuthCoverage` at `schema.go:48` is the nearest example of the same form.

- [ ] **Step 4: Seed the default and validate declarations**

In `schema.go`, add to `schemaBuilder`:

```go
	reqDirectives []reqDirective
```

Seed it where the builder is constructed, before options are applied, so the Apollo spelling is always present and a user redeclaring it collides:

```go
	b.reqDirectives = []reqDirective{{name: authDirective, arg: "scopes", shape: ScopesNested}}
```

In `authz_shape.go`:

```go
// validateRequirementDirectives rejects a declaration that would read as "no
// requirement" on every site while the schema builds clean: an undeclared
// shape, a name the SDL never declares, an argument the directive does not
// have, or the same name twice.
func (b *schemaBuilder) validateRequirementDirectives() {
	seen := make(map[string]bool, len(b.reqDirectives))
	for _, rd := range b.reqDirectives {
		if seen[rd.name] {
			b.errorf("requirement directive @%s is already declared", rd.name)
			continue
		}
		seen[rd.name] = true
		if rd.shape < ScopesNested || rd.shape > ScopesAnyOf {
			b.errorf("requirement directive @%s: shape must be declared as ScopesNested, ScopesAllOf or ScopesAnyOf", rd.name)
			continue
		}
		def, ok := b.ast.Directives[rd.name]
		if !ok {
			b.errorf("requirement directive: no directive @%s is declared in the SDL", rd.name)
			continue
		}
		if def.Arguments.ForName(rd.arg) == nil {
			b.errorf("requirement directive @%s has no argument %q", rd.name, rd.arg)
		}
	}
}
```

Call it in `build()` **before** `validateAuthDirectives()`, so no later validator walks a malformed declaration.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test -count=1 -run TestRequirementDirective -v .`
Expected: all subtests PASS.

- [ ] **Step 6: Confirm nothing else regressed**

Run: `go vet ./... && go test -race -count=1 .`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add authz.go schema.go authz_shape.go authz_directive_test.go
git commit -m "feat: declare requirement directives and reject a bad declaration"
```

---

### Task 2: Read every declared shape into a Requirement

**Files:**
- Modify: `authz_shape.go` (`requirementOf` and its three call sites)
- Test: `authz_directive_test.go` (append)

**Interfaces:**
- Consumes: `reqDirective`, `schemaBuilder.reqDirectives`, the three `Scopes*` constants.
- Produces: `func (b *schemaBuilder) requirementOf(ds ast.DirectiveList) (Requirement, bool, bool)` — the package-level function becomes a method, same return contract; `func groupsOf(arg *ast.Argument, shape ScopeShape) [][]string`.

- [ ] **Step 1: Write the failing test**

Append to `authz_directive_test.go`, and add `"context"` to its imports:

```go
// scopesOf renders a coordinate's effective requirement as its groups, so a
// test asserts the composition rather than a boolean.
func scopesOf(t *testing.T, s *Schema, typeName, field string) [][]string {
	t.Helper()
	obj := s.objects[typeName]
	if obj == nil {
		t.Fatalf("no object %q", typeName)
	}
	fd := obj.fields[field]
	if fd == nil {
		t.Fatalf("no field %s.%s", typeName, field)
	}
	return fd.requires.anyOf
}

func sameGroups(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				return false
			}
		}
	}
	return true
}

// The same SDL must read differently under each shape. This is the assertion
// the design turns on: the engine cannot infer AND vs OR, so it must honour
// what was declared.
func TestScopeShapeChangesTheRequirement(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on FIELD_DEFINITION
type Query { secret: String! @auth(requires: ["a","b"]) }
`
	for _, c := range []struct {
		shape ScopeShape
		want  [][]string
	}{
		{ScopesAllOf, [][]string{{"a", "b"}}},
		{ScopesAnyOf, [][]string{{"a"}, {"b"}}},
	} {
		s, err := NewSchema(SDL(sdl),
			RequirementDirective("auth", "requires", c.shape),
			Query(Field("secret", func(Root) string { return "s" })))
		if err != nil {
			t.Fatalf("shape %d: NewSchema: %v", c.shape, err)
		}
		if got := scopesOf(t, s, "Query", "secret"); !sameGroups(got, c.want) {
			t.Fatalf("shape %d: groups = %v, want %v", c.shape, got, c.want)
		}
	}
}

// Both spellings on one coordinate compose the way repeated occurrences of one
// directive already do: ANDed. More declarations never mean less restrictive.
func TestBothSpellingsOnOneFieldAnd(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on FIELD_DEFINITION
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION
type Query { secret: String! @auth(requires: ["a"]) @requiresScopes(scopes: [["b"]]) }
`
	s, err := NewSchema(SDL(sdl),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	got := scopesOf(t, s, "Query", "secret")
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("groups = %v, want one group of two (a AND b)", got)
	}
}

// End to end: a custom directive must be enforced, not merely recorded.
func TestCustomDirectiveIsEnforced(t *testing.T) {
	s, err := NewSchema(SDL(legacySDL),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	held := map[string]bool{}
	e := NewExecutor(s, WithAuthorizer(ScopeAuthorizer(
		func(context.Context) map[string]bool { return held })))
	resp := run(t, e, `{ secret }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a guarded field resolved without the scope")
	}
	held["admin"] = true
	if resp = run(t, e, `{ secret }`, ""); len(resp.Errors) != 0 {
		t.Fatalf("holding the scope still denied: %v", resp.Errors)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test -count=1 -run 'TestScopeShape|TestBothSpellings|TestCustomDirectiveIsEnforced' . 2>&1 | head -20`

Expected: FAIL. `TestScopeShapeChangesTheRequirement` reports empty groups, because `requirementOf` still reads only `@requiresScopes`. A compile error here means Step 1 was mistyped.

- [ ] **Step 3: Convert requirementOf to a method over the declared shapes**

Replace the body of `requirementOf` in `authz_shape.go`. Keep its doc comment, and keep the `(req, ok, capped)` contract exactly — every caller depends on `capped` meaning "req is not the true value".

```go
func (b *schemaBuilder) requirementOf(ds ast.DirectiveList) (req Requirement, ok bool, capped bool) {
	for _, rd := range b.reqDirectives {
		for _, d := range ds.ForNames(rd.name) {
			arg := d.Arguments.ForName(rd.arg)
			if arg == nil || arg.Value == nil {
				continue
			}
			groups := groupsOf(arg, rd.shape)
			if groups == nil {
				continue
			}
			ok = true
			combined, within := andCapped(req, NewRequirement(groups...))
			if !within {
				return req, true, true
			}
			req = combined
		}
	}
	return req, ok, false
}

// groupsOf reads one argument into requirement groups according to the declared
// shape. nil means the literal does not match the shape;
// checkRequirementDirectives reports that as a build error, so nil here is
// "already reported" rather than "no requirement".
func groupsOf(arg *ast.Argument, shape ScopeShape) [][]string {
	switch shape {
	case ScopesNested:
		var groups [][]string
		for _, outer := range arg.Value.Children {
			var group []string
			for _, inner := range outer.Value.Children {
				group = append(group, inner.Value.Raw)
			}
			groups = append(groups, group)
		}
		return groups
	case ScopesAllOf:
		var group []string
		for _, elem := range arg.Value.Children {
			group = append(group, elem.Value.Raw)
		}
		if group == nil {
			return nil
		}
		return [][]string{group}
	case ScopesAnyOf:
		var groups [][]string
		for _, elem := range arg.Value.Children {
			groups = append(groups, []string{elem.Value.Raw})
		}
		return groups
	}
	return nil
}
```

`ok` now becomes true only when an occurrence yields groups, across all declared directives. Check every caller still reads it as "this coordinate declares something".

- [ ] **Step 4: Update the call sites**

Find them with `grep -n "requirementOf(" authz_shape.go`. Each becomes `b.requirementOf(...)`. They are already methods on `*schemaBuilder`, so nothing ripples outward.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test -count=1 -run 'TestScopeShape|TestBothSpellings|TestCustomDirectiveIsEnforced' -v .`
Expected: PASS.

- [ ] **Step 6: Confirm the existing suite is unchanged**

Run: `go vet ./... && go test -race -count=1 .`
Expected: `ok`. Every pre-existing authorization test exercises the default entry and must behave exactly as the constant did.

- [ ] **Step 7: Commit**

```bash
git add authz_shape.go authz_directive_test.go
git commit -m "feat: read every declared requirement directive"
```

---

### Task 3: Give every declared directive the same safety nets

This is the task the design exists for. A directive that is read but not guarded is worse than one not read at all: the author believes a requirement is in force.

**Files:**
- Modify: `authz_shape.go` (`checkRequiresScopes`, `rejectUnenforced`, the cap error)
- Test: `authz_directive_test.go` (append)

**Interfaces:**
- Consumes: `reqDirective`, `groupsOf`, `schemaBuilder.reqDirectives`, and `requirementOf` as Task 2 left it — `(Requirement, bool, bool)`.
- Produces: `func (b *schemaBuilder) checkRequirementDirectives(coord string, ds ast.DirectiveList)` replacing `checkRequiresScopes`; `func flatScopesValid(arg *ast.Argument) bool`. **Step 5 changes `requirementOf`'s third return from `bool` to `string`** (`cappedBy`, empty when not capped), so its three call sites and the `authCapped` bookkeeping move with it. Nothing outside `authz_shape.go` sees that signature.

- [ ] **Step 1: Write the failing test**

Append to `authz_directive_test.go`, and add `"fmt"` to its imports:

```go
// gqlparser checks a directive's name, location and argument presence, never
// its argument literal. Without a per-shape check, @auth(requires: "admin") --
// a string where a list belongs -- builds clean and guards nothing.
func TestCustomDirectiveMalformedLiteralIsRejected(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on FIELD_DEFINITION
type Query { secret: String! @auth(requires: "admin") }
`
	_, err := NewSchema(SDL(sdl),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil {
		t.Fatal("a malformed literal was accepted")
	}
	if !strings.Contains(err.Error(), "@auth") {
		t.Fatalf("error does not name the offending directive: %v", err)
	}
}

// A custom directive in a position the engine does not enforce must be a build
// error naming that directive, exactly as @requiresScopes is. Reading a
// directive without also rejecting its misplacement is the fail-open this
// design exists to prevent.
func TestCustomDirectiveMisplacementIsRejected(t *testing.T) {
	for _, c := range []struct{ name, sdl string }{
		{"input field", `
directive @auth(requires: [String!]) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
input Where { name: String @auth(requires: ["admin"]) }
type Query { secret(w: Where): String! }
`},
		{"field argument", `
directive @auth(requires: [String!]) on FIELD_DEFINITION | ARGUMENT_DEFINITION
type Query { secret(id: ID! @auth(requires: ["admin"])): String! }
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(SDL(c.sdl),
				RequirementDirective("auth", "requires", ScopesAllOf))
			if err == nil {
				t.Fatal("a misplaced custom directive was accepted")
			}
			if !strings.Contains(err.Error(), "@auth") {
				t.Fatalf("error does not name the offending directive: %v", err)
			}
		})
	}
}

// The group cap protects plan compile from a combinatorial requirement. It must
// apply to every spelling, and say which one overflowed.
func TestCustomDirectiveIsCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("directive @auth(requires: [String!]) repeatable on FIELD_DEFINITION\ntype Query {\n  secret: String!")
	for i := range maxRequirementGroups + 1 {
		fmt.Fprintf(&sb, " @auth(requires: [\"s%d\"])", i)
	}
	sb.WriteString("\n}\n")
	_, err := NewSchema(SDL(sb.String()),
		RequirementDirective("auth", "requires", ScopesAnyOf),
		Query(Field("secret", func(Root) string { return "s" })))
	if err == nil {
		t.Fatal("an over-cap requirement was accepted")
	}
	if !strings.Contains(err.Error(), "@auth") {
		t.Fatalf("cap error does not name the offending directive: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test -count=1 -run 'TestCustomDirectiveMalformed|TestCustomDirectiveMisplacement|TestCustomDirectiveIsCapped' . 2>&1 | head -20`

Expected: FAIL, each reporting that a bad schema was accepted. Confirm you see "was accepted" and not a compile error — the "accepted" failures are the fail-open the spec's §5 describes.

- [ ] **Step 3: Make the literal check per shape**

In `authz_shape.go`, replace `checkRequiresScopes` with:

```go
// checkRequirementDirectives shape-checks every occurrence of every declared
// requirement directive on coord, not just the first match: a repeatable
// directive or an extension can add a second occurrence gqlparser never folds
// into the first (see requirementOf), and a malformed one must fail the build
// exactly like a malformed first occurrence would.
func (b *schemaBuilder) checkRequirementDirectives(coord string, ds ast.DirectiveList) {
	for _, rd := range b.reqDirectives {
		for _, d := range ds.ForNames(rd.name) {
			arg := d.Arguments.ForName(rd.arg)
			valid := false
			switch rd.shape {
			case ScopesNested:
				valid = scopesShapeValid(arg)
			case ScopesAllOf, ScopesAnyOf:
				valid = flatScopesValid(arg)
			}
			if valid {
				continue
			}
			if rd.shape == ScopesNested {
				b.errorf("%s: @%s %s must be a non-empty list of non-empty lists of strings", coord, rd.name, rd.arg)
			} else {
				b.errorf("%s: @%s %s must be a non-empty list of strings", coord, rd.name, rd.arg)
			}
		}
	}
}

// flatScopesValid accepts a non-empty list of string literals, which is what
// ScopesAllOf and ScopesAnyOf read.
func flatScopesValid(arg *ast.Argument) bool {
	if arg == nil || arg.Value == nil || arg.Value.Kind != ast.ListValue || len(arg.Value.Children) == 0 {
		return false
	}
	for _, elem := range arg.Value.Children {
		if elem.Value == nil || elem.Value.Kind != ast.StringValue {
			return false
		}
	}
	return true
}
```

Update both `checkRequiresScopes(...)` call sites in `validateAuthDirectives` to `b.checkRequirementDirectives(...)`.

- [ ] **Step 4: Make misplacement rejection cover every declared name**

```go
func (b *schemaBuilder) rejectUnenforced(coord, what string, ds ast.DirectiveList) {
	for _, rd := range b.reqDirectives {
		if ds.ForName(rd.name) != nil {
			b.errorf("%s: @%s is not enforced on %s; declare it on an object, interface or field", coord, rd.name, what)
		}
	}
}
```

- [ ] **Step 5: Make the cap error name the directive that overflowed**

Find it with `grep -n "more than %d groups" authz_shape.go`. It hardcodes `authDirective` and must name the directive whose occurrences overflowed.

Thread the name out of `requirementOf` rather than recording it on the builder — a builder field outlives the coordinate it describes. Change the signature to `(req Requirement, ok bool, cappedBy string)`, where `""` means not capped, and update the three call sites and the `authCapped` bookkeeping. Keep the doc comment's contract sentence accurate after the change.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test -count=1 -run TestCustomDirective -v .`
Expected: PASS.

- [ ] **Step 7: Prove each safety net has teeth**

For each, break it, confirm the matching test fails, then restore with a reverse edit — never `git checkout --`, because the file has uncommitted work.

1. In `checkRequirementDirectives`, set `valid = true` unconditionally in the flat branch → `TestCustomDirectiveMalformedLiteralIsRejected` must FAIL.
2. In `rejectUnenforced`, iterate `b.reqDirectives[:1]` → `TestCustomDirectiveMisplacementIsRejected` must FAIL.
3. In the cap error, hardcode the name back to `authDirective` → `TestCustomDirectiveIsCapped` must FAIL on the name assertion.

Record the three observed failure messages in the commit body.

- [ ] **Step 8: Run the full suite**

Run: `go vet ./... && go test -race -count=1 .`
Expected: `ok`.

- [ ] **Step 9: Commit**

```bash
git add authz_shape.go authz_directive_test.go
git commit -m "fix: give every declared requirement directive the same safety nets"
```

---

### Task 4: Inheritance, unchanged defaults, and the cost check

**Files:**
- Test: `authz_directive_test.go` (append)
- Modify: `CLAUDE.md`; `docs/superpowers/specs/2026-09-16-authorization-design.md` §8

**Interfaces:**
- Consumes: everything from Tasks 1–3. Produces nothing new.

- [ ] **Step 1: Write the failing test**

Append to `authz_directive_test.go`:

```go
type authDirDoc struct {
	ID   string
	Body string
}

// A custom directive must inherit exactly as @requiresScopes does: a field's
// own requirement ANDed with its object type's and each implemented
// interface's. resolveAuthRequirements owns that logic and must not have been
// bypassed by the new reading path.
func TestCustomDirectiveInherits(t *testing.T) {
	const sdl = `
directive @auth(requires: [String!]) on OBJECT | INTERFACE | FIELD_DEFINITION
interface Node @auth(requires: ["node"]) { id: ID! }
type Doc implements Node @auth(requires: ["doc"]) {
  id: ID!
  body: String! @auth(requires: ["body"])
}
type Query { doc: Doc! }
`
	s, err := NewSchema(SDL(sdl),
		RequirementDirective("auth", "requires", ScopesAllOf),
		Object[authDirDoc]("Doc",
			Field("id", func(d *authDirDoc) ID { return ID(d.ID) }),
			Field("body", func(d *authDirDoc) string { return d.Body }),
		),
		Query(Field("doc", func(Root) authDirDoc { return authDirDoc{ID: "1"} })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	got := scopesOf(t, s, "Doc", "body")
	if len(got) != 1 {
		t.Fatalf("groups = %v, want a single ANDed group", got)
	}
	has := map[string]bool{}
	for _, sc := range got[0] {
		has[sc] = true
	}
	for _, want := range []string{"body", "doc", "node"} {
		if !has[want] {
			t.Fatalf("effective requirement %v is missing %q", got, want)
		}
	}
}

// With no option supplied, the default entry must behave exactly as the
// constant did. This is the regression guard for every existing schema.
func TestDefaultSpellingUnchanged(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION
type Query { secret: String! @requiresScopes(scopes: [["a","b"],["c"]]) }
`
	s, err := NewSchema(SDL(sdl), Query(Field("secret", func(Root) string { return "s" })))
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	if got := scopesOf(t, s, "Query", "secret"); !sameGroups(got, [][]string{{"a", "b"}, {"c"}}) {
		t.Fatalf("groups = %v, want [[a b] [c]]", got)
	}
}
```

If the interface `Node` needs an explicit binding for this schema to build, copy the form `authz_inherit_test.go` uses for an unbound interface rather than guessing.

- [ ] **Step 2: Run the tests**

Run: `go test -count=1 -run 'TestCustomDirectiveInherits|TestDefaultSpellingUnchanged' -v .`
Expected: PASS if Tasks 1–3 are correct. A failure means the new reading path bypassed `resolveAuthRequirements` — fix that, do not weaken the test.

- [ ] **Step 3: Confirm the cost claim**

This work is `NewSchema`-time, so the request path must measure identical.

```bash
go test -c -o /tmp/after.exe .
git stash push -u -m "reqdir-bench" -q
go test -c -o /tmp/before.exe .
SHA=$(git stash list --format='%H %gs' | grep reqdir-bench | head -1 | awk '{print $1}')
git stash apply -q "$SHA"
git stash drop -q "$(git stash list | grep reqdir-bench | head -1 | cut -d: -f1)"
rm -f /tmp/b.txt /tmp/a.txt
for i in $(seq 1 10); do
  /tmp/before.exe -test.run xxx -test.bench 'BenchmarkExecuteWithAuthorizer|BenchmarkFieldPathBare' -test.benchmem -test.count=1 >> /tmp/b.txt
  /tmp/after.exe  -test.run xxx -test.bench 'BenchmarkExecuteWithAuthorizer|BenchmarkFieldPathBare' -test.benchmem -test.count=1 >> /tmp/a.txt
done
benchstat /tmp/b.txt /tmp/a.txt
```

Expected: allocations identical sample for sample. Record the figures in the commit body. Then run `go test -count=1 -run TestStructSizes -v .` and confirm 64 / 160 / 176.

- [ ] **Step 4: Correct the spec that started this**

In `docs/superpowers/specs/2026-09-16-authorization-design.md` §8, the sentence "P1 accepts any directive that yields a `Requirement`, so the core package is indifferent" is now true, but only because of this work. Replace the claim with what the code does and link the new design doc.

- [ ] **Step 5: Document it in CLAUDE.md**

Add to the authorization section: the default entry; that the option adds rather than replaces; that two spellings on one coordinate AND; and — the part a future reader most needs — that the four welded places move together, and that reading a directive without rejecting its misplacement is the fail-open the design exists to prevent. Name the three tests that hold it.

- [ ] **Step 6: Full gate**

```bash
sh scripts/gate.sh
golangci-lint run ./...
```

Expected: four modules ok, 0 issues.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "docs: record pluggable requirement directives and correct section 8"
```
