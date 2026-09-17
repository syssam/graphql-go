# Authorization 2b: Argument Sites Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an Authorizer refuse a field because of what the client put in its filter, ordering or write arguments — keys, enum values and explicit nulls — so a restricted column cannot be filtered, sorted, grouped or written through.

**Architecture:** `@authorizeInput(kind: FILTER | WRITE)` on an argument definition becomes a per-plan argument site. Before `Authorize`, the engine walks each argument site's *supplied* value (AST plus variables, typed against the schema) into `[]InputKey` and exposes it on the per-request `Decision`. `Deny` on an argument site refuses the field before its resolver. A field with argument sites always carries an output site so the existing single `authIdx >= 0` compare routes it into `enforceAuth`; fields without them pay nothing new.

**Tech Stack:** Go 1.27, `github.com/vektah/gqlparser/v2`.

**Spec:** `docs/superpowers/specs/2026-09-16-authorization-design.md` — §9.4 (D1–D9) is this plan's design; §9.3 is superseded where §9.4 corrects it; §5–§7 still bind.

## Global Constraints

- Root package may depend only on `github.com/vektah/gqlparser/v2` and the standard library.
- No reflection on the request hot path.
- Do not grow `execState` (64 bytes) or `OperationContext` (160 bytes). `planField` is per plan and may grow.
- Go 1.27. Tests live beside the code in `package graphql`.
- `-race` is not optional. The gate is `sh scripts/gate.sh -short` (four modules).
- Comments explain why, not what. English only. No history-narrating comments.
- Commit messages: imperative, lower-case prefix, ending with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Stage files explicitly; never `git add -A`. Never bare `git stash` / `git stash pop`.
- Every security-relevant check ships with a test proven to fail when the check is removed. Record that failure in the task report.
- Scalar input values (strings, numbers, booleans) must never appear in anything reported to an Authorizer.

## File Structure

| File | Change |
|---|---|
| `authz_input.go` (new) | `InputKey`, `inputKeys` — the typed walk of a supplied argument. Pure, no executor dependency. |
| `authz_input_test.go` (new) | Every test this plan adds. |
| `authz.go` | `AuthSite.Arg` + unexported arg fields; `AuthShape.hasArgSites`; `Decision.inputs` / `Decision.Input`; `validFor` rule for argument sites. |
| `authz_shape.go` | `inputDirective` constants; `validateInputDirectives`; shape builder adds argument sites and the routing output site. |
| `schema.go` | `build()` calls `validateInputDirectives`. |
| `plan.go` | `planField.argSites []int32`. |
| `exec.go` | `authorize`/`runAuthorizer` take variables and fill `Decision.inputs`. |
| `authz_exec.go` | `enforceAuth` checks argument sites first. |
| `subscription.go` | Open handler refuses on an argument-site Deny; passes variables to `authorize`. |
| `doc.go`, `CLAUDE.md` | Documentation. |

---

### Task 1: `InputKey` and the supplied-value walk

**Files:**
- Create: `authz_input.go`, `authz_input_test.go`

**Interfaces:**
- Produces: `type InputKey struct { Path []string; Enum string; Null bool }`; `func inputKeys(types map[string]*ast.Definition, t *ast.Type, v *ast.Value, vars map[string]any) []InputKey`.

- [ ] **Step 1: Write the failing tests**

Create `authz_input_test.go`:

```go
package graphql

import (
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

const inputSDL = `
enum OrderField { NAME TAX_NUMBER }
enum Direction { ASC DESC }
input Order { field: OrderField! direction: Direction = ASC }
input Where { not: Where and: [Where!] nameContains: String taxNumber: String }
input Patch { name: String taxNumber: String }
type Query { customers(where: Where, orderBy: [Order!], groupBy: [OrderField!]): [String!]! }
type Mutation { update(patch: Patch!): String }
`

// supplied parses a query and returns the argument's type, supplied value and
// the schema's types, so each case states the operation exactly as a client
// would send it.
func supplied(t *testing.T, query, field, arg string) (map[string]*ast.Definition, *ast.Type, *ast.Value) {
	t.Helper()
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: inputSDL})
	doc, errs := gqlparser.LoadQuery(schema, query)
	if errs != nil {
		t.Fatalf("query: %v", errs)
	}
	sel := doc.Operations[0].SelectionSet[0].(*ast.Field)
	if sel.Name != field {
		t.Fatalf("first selection is %s, want %s", sel.Name, field)
	}
	def := sel.Definition.Arguments.ForName(arg)
	var v *ast.Value
	if a := sel.Arguments.ForName(arg); a != nil {
		v = a.Value
	}
	return schema.Types, def.Type, v
}

func render(keys []InputKey) []string {
	var out []string
	for _, k := range keys {
		s := strings.Join(k.Path, ".")
		if k.Enum != "" {
			s += "=" + k.Enum
		}
		if k.Null {
			s += "=null"
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestInputKeys(t *testing.T) {
	cases := []struct {
		name  string
		query string
		field string
		arg   string
		vars  map[string]any
		want  []string
	}{
		{"filter keys, scalar values never reported",
			`{ customers(where: {nameContains: "ada", taxNumber: "123"}) }`, "customers", "where", nil,
			[]string{"nameContains", "taxNumber"}},
		{"nested and list, indices omitted, deduplicated",
			`{ customers(where: {and: [{taxNumber: "1"}, {taxNumber: "2"}], not: {nameContains: "x"}}) }`, "customers", "where", nil,
			[]string{"and", "and.taxNumber", "not", "not.nameContains"}},
		{"ordering by enum value",
			`{ customers(orderBy: [{field: TAX_NUMBER}]) }`, "customers", "orderBy", nil,
			[]string{"field=TAX_NUMBER"}},
		{"SDL input-field default is not reported",
			`{ customers(orderBy: [{field: NAME}]) }`, "customers", "orderBy", nil,
			[]string{"field=NAME"}},
		{"enum list argument reports each value",
			`{ customers(groupBy: [NAME, TAX_NUMBER]) }`, "customers", "groupBy", nil,
			[]string{"=NAME", "=TAX_NUMBER"}},
		{"variable object typed against the schema",
			`query($o: [Order!]) { customers(orderBy: $o) }`, "customers", "orderBy",
			map[string]any{"o": []any{map[string]any{"field": "TAX_NUMBER", "direction": "DESC"}}},
			[]string{"direction=DESC", "field=TAX_NUMBER"}},
		{"variable nested inside a literal",
			`query($f: OrderField!) { customers(orderBy: [{field: $f}]) }`, "customers", "orderBy",
			map[string]any{"f": "TAX_NUMBER"},
			[]string{"field=TAX_NUMBER"}},
		{"absent variable reports nothing",
			`query($w: Where) { customers(where: $w) }`, "customers", "where", map[string]any{},
			nil},
		{"explicit null key is reported",
			`mutation { update(patch: {taxNumber: null}) }`, "update", "patch", nil,
			[]string{"taxNumber=null"}},
		{"absent key is not a write",
			`mutation { update(patch: {name: "a"}) }`, "update", "patch", nil,
			[]string{"name"}},
		{"explicit null through a variable",
			`mutation($p: Patch!) { update(patch: $p) }`, "update", "patch",
			map[string]any{"p": map[string]any{"taxNumber": nil}},
			[]string{"taxNumber=null"}},
		{"argument not supplied",
			`{ customers }`, "customers", "where", nil,
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			types, typ, v := supplied(t, tc.query, tc.field, tc.arg)
			var got []InputKey
			if v != nil {
				got = inputKeys(types, typ, v, tc.vars)
			}
			if r := render(got); !slices.Equal(r, tc.want) {
				t.Errorf("keys = %v, want %v", r, tc.want)
			}
		})
	}
}

// A variable's value never appears in a key: only paths and enum identifiers.
func TestInputKeysNeverCarryScalarValues(t *testing.T) {
	types, typ, v := supplied(t, `query($w: Where) { customers(where: $w) }`, "customers", "where")
	keys := inputKeys(types, typ, v, map[string]any{"w": map[string]any{"taxNumber": "SECRET-123"}})
	for _, k := range keys {
		if strings.Contains(strings.Join(k.Path, "."), "SECRET") || strings.Contains(k.Enum, "SECRET") {
			t.Fatalf("a scalar value leaked into %+v", k)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestInputKeys' .`
Expected: build failure, `undefined: InputKey`, `undefined: inputKeys`.

- [ ] **Step 3: Implement**

Create `authz_input.go`:

```go
package graphql

import (
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// InputKey is one position a client supplied inside an argument that an
// Authorizer is asked about: the input-object keys from the argument down,
// list indices omitted. Enum holds the value when that position is an enum,
// because ordering and grouping name a column by enum value rather than by
// key. Null is set for an explicit null, which is a write where an absent key
// is not. Scalar values are never carried: a policy decides on schema
// identifiers, and the values are user data.
type InputKey struct {
	Path []string
	Enum string
	Null bool
}

// inputKeys walks the value a client supplied for one argument. It reads the
// operation's AST and variables rather than an argument map, because an
// argument map has SDL defaults filled in and a default is not something the
// client chose. A default on an operation variable is, so a variable resolved
// from its default is walked like any other.
func inputKeys(types map[string]*ast.Definition, t *ast.Type, v *ast.Value, vars map[string]any) []InputKey {
	w := inputWalk{types: types, vars: vars, seen: map[string]bool{}}
	w.ast(t, v, nil)
	return w.keys
}

type inputWalk struct {
	types map[string]*ast.Definition
	vars  map[string]any
	keys  []InputKey
	seen  map[string]bool
}

func (w *inputWalk) add(path []string, enum string, null bool) {
	id := strings.Join(path, "\x00") + "\x01" + enum
	if null {
		id += "\x02"
	}
	if w.seen[id] {
		return
	}
	w.seen[id] = true
	w.keys = append(w.keys, InputKey{Path: slices.Clone(path), Enum: enum, Null: null})
}

func (w *inputWalk) ast(t *ast.Type, v *ast.Value, path []string) {
	if v == nil {
		return
	}
	switch v.Kind {
	case ast.Variable:
		raw, ok := w.vars[v.Raw]
		if !ok {
			return
		}
		w.raw(t, raw, path)
	case ast.NullValue:
		w.add(path, "", true)
	case ast.ListValue:
		elem := t
		if t.Elem != nil {
			elem = t.Elem
		}
		for _, c := range v.Children {
			w.ast(elem, c.Value, path)
		}
	case ast.ObjectValue:
		def := w.types[t.Name()]
		if def == nil {
			return
		}
		for _, c := range v.Children {
			fd := def.Fields.ForName(c.Name)
			if fd == nil {
				continue
			}
			child := append(slices.Clone(path), c.Name)
			if c.Value != nil && (c.Value.Kind == ast.ObjectValue || c.Value.Kind == ast.ListValue) {
				w.add(child, "", false)
			}
			w.ast(fd.Type, c.Value, child)
		}
	case ast.EnumValue:
		w.add(path, v.Raw, false)
	default:
		if len(path) > 0 {
			w.add(path, "", false)
		}
	}
}

func (w *inputWalk) raw(t *ast.Type, v any, path []string) {
	if v == nil {
		w.add(path, "", true)
		return
	}
	if t.Elem != nil {
		if list, ok := v.([]any); ok {
			for _, e := range list {
				w.raw(t.Elem, e, path)
			}
			return
		}
		// Input coercion accepts a single value where a list is expected.
		w.raw(t.Elem, v, path)
		return
	}
	def := w.types[t.Name()]
	if def == nil {
		return
	}
	switch def.Kind {
	case ast.Enum:
		if s, ok := v.(string); ok {
			w.add(path, s, false)
		}
	case ast.InputObject:
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for name, fv := range m {
			fd := def.Fields.ForName(name)
			if fd == nil {
				continue
			}
			child := append(slices.Clone(path), name)
			switch fv.(type) {
			case map[string]any, []any:
				w.add(child, "", false)
			}
			w.raw(fd.Type, fv, child)
		}
	default:
		if len(path) > 0 {
			w.add(path, "", false)
		}
	}
}
```

The literal and variable branches follow one rule: a key whose value is an object or a list is itself reported (`not`, `and`), and a leaf reports once — its path, its enum value, or its null — never twice. `TestInputKeys/nested_and_list` pins the first half; `ordering by enum value` and `explicit null key is reported` pin the second (a duplicate bare `field` or `taxNumber` entry would fail them).

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race -run 'TestInputKeys' .`
Expected: PASS.

- [ ] **Step 5: Prove the tests can fail**

(a) In `ast()`'s `ast.EnumValue` case, drop `v.Raw` (pass `""`). `ordering by enum value` must FAIL. Revert.
(b) In `raw()`'s `ast.Enum` case, do the same. `variable object typed against the schema` must FAIL. Revert.
(c) In `raw()`, treat `nil` as absent (return without `add`). `explicit null through a variable` must FAIL. Revert.

- [ ] **Step 6: Commit**

```bash
git add authz_input.go authz_input_test.go
git commit -m "feat: walk the input a client supplied for an argument

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Validate `@authorizeInput` at `NewSchema`

**Files:**
- Modify: `authz_shape.go` (constants, `validateInputDirectives`), `schema.go` (`build()` phase 6)
- Test: `authz_input_test.go`

**Interfaces:**
- Produces: `const inputDirective = "authorizeInput"`; `func (b *schemaBuilder) validateInputDirectives()`.

- [ ] **Step 1: Write the failing tests**

Append to `authz_input_test.go`:

```go
const inputDirectiveSDL = `
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION | FIELD_DEFINITION | INPUT_FIELD_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
enum OrderField { NAME TAX_NUMBER }
input Where { nameContains: String }
`

func TestAuthorizeInputPlacementAtBuild(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr string // empty means the schema must build
	}{
		{"input object argument", `type Query { c(where: Where @authorizeInput(kind: FILTER)): String }`, ""},
		{"enum list argument", `type Query { c(groupBy: [OrderField!] @authorizeInput(kind: FILTER)): String }`, ""},
		{"scalar argument", `type Query { c(q: String @authorizeInput(kind: FILTER)): String }`, "Query.c(q:)"},
		{"on a field", `type Query { c: String @authorizeInput(kind: FILTER) }`, "Query.c"},
		{"on an input field", `input Bad { a: Where @authorizeInput(kind: FILTER) } type Query { c(b: Bad): String }`, "Bad.a"},
		{"on an interface field argument", `interface Node { c(where: Where @authorizeInput(kind: FILTER)): String } type Query { c: String }`, "Node.c(where:)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchema(SDL(inputDirectiveSDL+tc.extra), Query(Field("c", func(Root) *string { return nil })))
			if tc.wantErr == "" {
				if err != nil && strings.Contains(err.Error(), "authorizeInput") {
					t.Fatalf("valid placement rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "authorizeInput") {
				t.Fatalf("want an @authorizeInput error naming %q, got %v", tc.wantErr, err)
			}
		})
	}
}
```

The valid cases may still fail `NewSchema` for unrelated reasons (an unbound argument, for example); they only assert that no `authorizeInput` error is among them. Confirm that by reading one valid case's error text.

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race -run 'TestAuthorizeInputPlacementAtBuild' .`
Expected: the four rejection cases FAIL; the two valid ones PASS. Record.

- [ ] **Step 3: Implement**

In `authz_shape.go`, beside `authDirective`:

```go
// inputDirective marks an argument whose supplied keys and enum values an
// Authorizer is shown before the field runs.
const inputDirective = "authorizeInput"
```

and add:

```go
// validateInputDirectives accepts @authorizeInput only on an argument of an
// object type's field whose named type is an input object or an enum: those
// are the only values with keys or enum identifiers to report. Anywhere else
// it would read as guarded while reporting nothing -- an interface field's
// argument is not enforced because the plan reads the concrete field's own
// definition, and a directive definition's argument is never executed.
func (b *schemaBuilder) validateInputDirectives() {
	reject := func(coord, what string, ds ast.DirectiveList) {
		if ds.ForName(inputDirective) != nil {
			b.errorf("%s: @%s is valid only on an object field's argument of input object or enum type, not on %s", coord, inputDirective, what)
		}
	}
	reject("schema", "the schema definition", b.ast.SchemaDirectives)
	for dname, ddef := range b.ast.Directives {
		for _, a := range ddef.Arguments {
			reject(argCoordinate("@"+dname, a.Name), "a directive argument", a.Directives)
		}
	}
	for name, def := range b.ast.Types {
		if def.BuiltIn {
			continue
		}
		reject(name, "a type", def.Directives)
		for _, f := range def.Fields {
			coord := coordinate(name, f.Name)
			switch def.Kind {
			case ast.Object:
				reject(coord, "a field", f.Directives)
				for _, a := range f.Arguments {
					if a.Directives.ForName(inputDirective) == nil {
						continue
					}
					named := b.ast.Types[a.Type.Name()]
					if named == nil || (named.Kind != ast.InputObject && named.Kind != ast.Enum) {
						b.errorf("%s: @%s is valid only on an argument of input object or enum type, not %s", argCoordinate(coord, a.Name), inputDirective, a.Type.String())
					}
				}
			case ast.Interface:
				reject(coord, "an interface field", f.Directives)
				for _, a := range f.Arguments {
					reject(argCoordinate(coord, a.Name), "an interface field's argument; declare it on the implementing object's field", a.Directives)
				}
			default:
				reject(coord, "an input field", f.Directives)
			}
		}
		for _, v := range def.EnumValues {
			reject(coordinate(name, v.Name), "an enum value", v.Directives)
		}
	}
}
```

In `schema.go` phase 6, call `b.validateInputDirectives()` immediately after `b.validateAuthDirectives()`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race -run 'TestAuthorizeInputPlacementAtBuild|TestRequiresScopesOnAnUnenforcedLocation|TestNewSchemaRejectsMalformedScopes' .`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

Remove the named-type check (the `named == nil || ...` block). `scalar argument` must FAIL. Revert. Remove the `ast.Interface` argument rejection. `on an interface field argument` must FAIL. Revert.

- [ ] **Step 6: Commit**

```bash
git add authz_shape.go schema.go authz_input_test.go
git commit -m "feat: validate @authorizeInput placement at NewSchema

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Argument sites at plan compile

**Files:**
- Modify: `authz.go` (`AuthSite`, `AuthShape`), `plan.go` (`planField`), `authz_shape.go` (`shapeBuilder.field`)
- Test: `authz_input_test.go`

**Interfaces:**
- Consumes: `inputDirective` (Task 2).
- Produces: `AuthSite.Arg string`; unexported `AuthSite.argType *ast.Type`, `AuthSite.argValue *ast.Value`; `AuthShape.hasArgSites bool`; `planField.argSites []int32`.

- [ ] **Step 1: Write the failing tests**

Append to `authz_input_test.go` (add `"context"` to imports):

```go
const argSiteSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT | INTERFACE
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
enum OrderField { NAME TAX_NUMBER }
input Order { field: OrderField! }
input Where { nameContains: String taxNumber: String }
input Patch { name: String taxNumber: String }
type Customer { name: String! }
type Query {
  customers(where: Where @authorizeInput(kind: FILTER), orderBy: [Order!] @authorizeInput(kind: FILTER)): [Customer!]!
  guarded(where: Where @authorizeInput(kind: FILTER)): [Customer!]! @requiresScopes(scopes: [["c:read"]])
  plain: String
}
type Mutation { update(patch: Patch! @authorizeInput(kind: WRITE)): String }
`

type argCustomer struct{ Name string }

// argArgs holds every argument Query.customers, Query.guarded and
// Mutation.update take, so one Args registration serves them all.
type argArgs struct {
	Where   *map[string]any
	OrderBy []map[string]any
	Patch   map[string]any
}

var argResolverCalls atomic.Int64

func argSiteSchema(t testing.TB) *Schema {
	t.Helper()
	list := func(context.Context, Root, argArgs) ([]*argCustomer, error) {
		argResolverCalls.Add(1)
		return []*argCustomer{{Name: "ada"}}, nil
	}
	s, err := NewSchema(SDL(argSiteSDL),
		Query(
			ResolveArgs("customers", list),
			ResolveArgs("guarded", list),
			Field("plain", func(Root) *string { return nil }),
		),
		Mutation(ResolveArgs("update", func(context.Context, Root, argArgs) (*string, error) {
			argResolverCalls.Add(1)
			return nil, nil
		})),
		Object[argCustomer]("Customer", Field("name", func(c *argCustomer) string { return c.Name })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}
```

Before writing further tests, read how the fixture registers `Args[...]` and input objects (`input_test.go`, `fixture_test.go`). Adapt `argSiteSchema`'s bindings to what `NewSchema` actually requires for these argument types; the requirement is only that the four fields build and that the resolvers count their calls. Keep the SDL exactly as given.

```go
func TestArgumentSitesAtPlanCompile(t *testing.T) {
	e := NewExecutor(argSiteSchema(t))
	p, _, perrs := planForTest(t, e, `{ customers { name } plain }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	if !p.shape.hasArgSites {
		t.Fatal("shape does not record argument sites")
	}
	var coords []string
	for _, s := range p.shape.Sites() {
		if s.Kind == SiteFilterArg {
			coords = append(coords, s.Coord)
			if s.Arg == "" {
				t.Errorf("site %s has no Arg", s.Coord)
			}
		}
	}
	slices.Sort(coords)
	if want := []string{"Query.customers(orderBy:)", "Query.customers(where:)"}; !slices.Equal(coords, want) {
		t.Errorf("argument sites = %v, want %v (a site exists even when the argument is not supplied)", coords, want)
	}

	sel := p.sel.forType(p.root)
	for _, f := range sel.fields {
		switch f.name {
		case "customers":
			if f.authIdx < 0 {
				t.Error("a field with argument sites must carry an output site to route into enforceAuth")
			}
			if len(f.argSites) != 2 {
				t.Errorf("customers argSites = %v, want 2", f.argSites)
			}
		case "plain":
			if f.authIdx != -1 || len(f.argSites) != 0 {
				t.Errorf("plain field gained authorization state: authIdx=%d argSites=%v", f.authIdx, f.argSites)
			}
		}
	}
}

func TestPlanWithoutArgumentSitesIsUnchanged(t *testing.T) {
	e := NewExecutor(argSiteSchema(t))
	p, _, perrs := planForTest(t, e, `{ plain }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	if !p.shape.IsEmpty() {
		t.Errorf("a plan touching no guarded field or argument built sites: %v", p.shape.Sites())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestArgumentSitesAtPlanCompile|TestPlanWithoutArgumentSitesIsUnchanged' .`
Expected: build failure (`hasArgSites`, `Arg`, `argSites` undefined). Record.

- [ ] **Step 3: Implement**

In `authz.go`, add to `AuthSite` (exported field beside `Kind`):

```go
	// Arg names the argument for SiteFilterArg and SiteInputWrite sites.
	Arg string
```

and to its unexported block:

```go
	// argType and argValue are the argument's declared type and the value the
	// operation supplied (nil when it supplied none), read at decision time
	// against the request's variables.
	argType  *ast.Type
	argValue *ast.Value
```

Add to `AuthShape`:

```go
	// hasArgSites gates the per-request input walk, so a plan without
	// argument sites pays nothing for it.
	hasArgSites bool
```

In `plan.go`, add to `planField` next to `authIdx`:

```go
	// argSites indexes this field's argument sites in the plan's AuthShape.
	argSites []int32
```

In `authz_shape.go`, make `buildAuthShape` carry `hasArgSites` onto the returned shape (track it on `shapeBuilder`), and extend `shapeBuilder.field`: reset `f.argSites = nil` beside `f.authIdx = -1`, and after the existing `switch`, when `f.def != nil`:

```go
	if f.def != nil {
		for _, ad := range f.def.def.Arguments {
			d := ad.Directives.ForName(inputDirective)
			if d == nil {
				continue
			}
			kind := SiteFilterArg
			if k := d.Arguments.ForName("kind"); k != nil && k.Value != nil && k.Value.Raw == "WRITE" {
				kind = SiteInputWrite
			}
			var supplied *ast.Value
			if a := f.ast.Arguments.ForName(ad.Name); a != nil {
				supplied = a.Value
			}
			f.argSites = append(f.argSites, int32(len(b.sites)))
			b.sites = append(b.sites, AuthSite{
				Coord:    argCoordinate(coordinate(f.def.object.name, f.name), ad.Name),
				Field:    f.def.def,
				Object:   f.def.object.def,
				Kind:     kind,
				Arg:      ad.Name,
				argType:  ad.Type,
				argValue: supplied,
			})
			b.hasArgSites = true
		}
		// A field with argument sites must reach enforceAuth through the one
		// compare every field already pays; a zero requirement allows.
		if len(f.argSites) > 0 && f.authIdx < 0 {
			f.authIdx = int32(len(b.sites))
			b.sites = append(b.sites, AuthSite{
				Coord:  coordinate(f.def.object.name, f.name),
				Field:  f.def.def,
				Object: f.def.object.def,
				Kind:   SiteOutput,
				leaf:   f.def.leaf,
			})
		}
	}
```

Place this before `b.walk(f.target, f.abstract, f.sub)`. Confirm `argCoordinate`'s output format matches the test's `Query.customers(where:)`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestArgumentSitesAtPlanCompile|TestPlanWithoutArgumentSitesIsUnchanged|TestAuthShape|TestTypename|TestInherited' .`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

Remove the routing block (`if len(f.argSites) > 0 && f.authIdx < 0`). `TestArgumentSitesAtPlanCompile` must FAIL on the routing assertion. Revert.

- [ ] **Step 6: Run the full root package and commit**

Run: `go test -race -count=1 .` — PASS.

```bash
git add authz.go plan.go authz_shape.go authz_input_test.go
git commit -m "feat: build argument sites at plan compile

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Report inputs to the Authorizer and enforce Deny

**Files:**
- Modify: `authz.go` (`Decision`, `validFor`), `exec.go` (`authorize`, `runAuthorizer`, `runOperation`), `authz_exec.go` (`enforceAuth`), `subscription.go` (open handler)
- Test: `authz_input_test.go`

**Interfaces:**
- Consumes: `inputKeys` (Task 1); `AuthSite.argType/argValue`, `AuthShape.hasArgSites`, `planField.argSites` (Task 3).
- Produces: `func (d *Decision) Input(site int) []InputKey`; `authorize(ctx context.Context, p *plan, vars map[string]any)`.

- [ ] **Step 1: Write the failing tests**

Append to `authz_input_test.go` (add `"errors"` if needed):

```go
// taxPolicy denies any filter or ordering that touches taxNumber, and any write
// that sets it -- the consumer's field control in miniature.
func taxPolicy(seen *[]InputKey) Authorizer {
	return AuthorizerFunc(func(ctx context.Context, shape *AuthShape, d *Decision) error {
		for i, s := range shape.Sites() {
			if s.Kind != SiteFilterArg && s.Kind != SiteInputWrite {
				continue
			}
			for _, k := range d.Input(i) {
				if seen != nil {
					*seen = append(*seen, k)
				}
				if slices.Contains(k.Path, "taxNumber") || k.Enum == "TAX_NUMBER" {
					if err := d.Set(i, Deny("customer:taxNumber", s.Coord)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func TestArgumentSiteDenyRefusesTheField(t *testing.T) {
	cases := []struct {
		name  string
		query string
		vars  string
		deny  bool
	}{
		{"filtering by a restricted key", `{ customers(where: {taxNumber: "1"}) { name } }`, "", true},
		{"ordering by a restricted enum value", `{ customers(orderBy: [{field: TAX_NUMBER}]) { name } }`, "", true},
		{"ordering via a variable", `query($o: [Order!]) { customers(orderBy: $o) { name } }`, `{"o":[{"field":"TAX_NUMBER"}]}`, true},
		{"writing a restricted field to null", `mutation { update(patch: {taxNumber: null}) }`, "", true},
		{"an unrestricted filter runs", `{ customers(where: {nameContains: "a"}) { name } }`, "", false},
		{"an unrestricted write runs", `mutation { update(patch: {name: "a"}) }`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argResolverCalls.Store(0)
			e := NewExecutor(argSiteSchema(t), WithAuthorizer(taxPolicy(nil)))
			resp := run(t, e, tc.query, tc.vars)
			if !tc.deny {
				if len(resp.Errors) > 0 {
					t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
				}
				if argResolverCalls.Load() != 1 {
					t.Errorf("resolver ran %d times, want 1", argResolverCalls.Load())
				}
				return
			}
			if len(resp.Errors) != 1 {
				t.Fatalf("errors = %s, want exactly one denial", errorsJSON(resp.Errors))
			}
			if got := resp.Errors[0].Extensions["code"]; got != CodeForbidden {
				t.Errorf("code = %v, want %v", got, CodeForbidden)
			}
			if n := argResolverCalls.Load(); n != 0 {
				t.Errorf("resolver ran %d times; a denied argument must refuse before it runs", n)
			}
		})
	}
}

func TestArgumentSiteInputIsReportedOnlyWhenPlanHasArgumentSites(t *testing.T) {
	var seen []InputKey
	e := NewExecutor(argSiteSchema(t), WithAuthorizer(taxPolicy(&seen)))
	run(t, e, `{ customers(where: {nameContains: "SECRET"}, orderBy: [{field: NAME}]) { name } }`, "")
	got := render(seen)
	if !slices.Contains(got, "nameContains") || !slices.Contains(got, "field=NAME") {
		t.Errorf("reported inputs = %v", got)
	}
	for _, k := range got {
		if strings.Contains(k, "SECRET") {
			t.Errorf("a scalar value reached the Authorizer: %v", got)
		}
	}
}

func TestArgumentSiteAdmitsOnlyAllowAndDeny(t *testing.T) {
	for _, o := range []struct {
		name    string
		out     Outcome
		wantErr bool
	}{
		{"Allow", Allow(), false},
		{"Deny", Deny("p", "r"), false},
		{"Null", Null(), true},
		{"Zero", Zero(), true},
		{"Redact", Redact(func(v any) any { return v }), true},
	} {
		t.Run(o.name, func(t *testing.T) {
			var setErr error
			e := NewExecutor(argSiteSchema(t), WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					for i, s := range shape.Sites() {
						if s.Kind == SiteFilterArg {
							setErr = d.Set(i, o.out)
							return nil
						}
					}
					return nil
				})))
			run(t, e, `{ customers(where: {nameContains: "a"}) { name } }`, "")
			if (setErr != nil) != o.wantErr {
				t.Errorf("Set(%s) error = %v, wantErr %v", o.name, setErr, o.wantErr)
			}
		})
	}
}

// A field that carries both an output requirement and an argument site must
// honour both: an allowed output with a denied argument is still refused.
func TestArgumentDenyAppliesAlongsideAnOutputRequirement(t *testing.T) {
	argResolverCalls.Store(0)
	e := NewExecutor(argSiteSchema(t), WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			if err := ScopeAuthorizer(func(context.Context) map[string]bool {
				return map[string]bool{"c:read": true}
			}).Authorize(ctx, shape, d); err != nil {
				return err
			}
			return taxPolicy(nil).Authorize(ctx, shape, d)
		})))
	resp := run(t, e, `{ guarded(where: {taxNumber: "1"}) { name } }`, "")
	if len(resp.Errors) == 0 || argResolverCalls.Load() != 0 {
		t.Fatalf("argument denial ignored when the output site allowed: errors=%s calls=%d", errorsJSON(resp.Errors), argResolverCalls.Load())
	}
}
```

Subscription open: add a subscription root field with an `@authorizeInput` argument to a small schema of your own (the existing `newAuthSubGuardedExecutor` in `authz_test.go` is the model to follow, including its `opens` counter), and assert that a denied argument refuses `Subscribe` with a `*SubscribeError` carrying `CodeForbidden` and `opens == 0`, while an allowed argument opens.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race -run 'TestArgumentSite|TestArgumentDeny|TestSubscribe' .`
Expected: build failure (`d.Input` undefined), then after adding a stub the denial cases FAIL. Record.

- [ ] **Step 3: Implement**

In `authz.go`, add to `Decision`:

```go
	// inputs holds, per argument site, what the client supplied. Nil when the
	// plan has no argument sites.
	inputs [][]InputKey
```

and:

```go
// Input returns what the client supplied for an argument site: key paths,
// enum values and explicit nulls, never scalar values. It is empty for any
// other site and for an argument the client did not supply.
func (d *Decision) Input(site int) []InputKey {
	if d == nil || site < 0 || site >= len(d.inputs) {
		return nil
	}
	return d.inputs[site]
}
```

At the top of `validFor`, beside the `SiteObject` rule:

```go
	// An argument site decides whether the field may run with the input it
	// was given; there is no value of its own to null, zero or redact.
	if (site.Kind == SiteFilterArg || site.Kind == SiteInputWrite) && o.act != actionAllow && o.act != actionDeny {
		return Errorf("authorization: only Allow and Deny are valid for %s, an argument site", site.Coord)
	}
```

In `exec.go`, change `authorize` and `runAuthorizer` to take `vars map[string]any`, and in `runAuthorizer`, between `newDecision` and `Authorize`:

```go
	if p.shape.hasArgSites {
		d.inputs = make([][]InputKey, len(p.shape.sites))
		for i, s := range p.shape.sites {
			if s.argValue != nil {
				d.inputs[i] = inputKeys(e.schema.ast.Types, s.argType, s.argValue, vars)
			}
		}
	}
```

The walk is inside `runAuthorizer`'s recover, so a panic in it is handled like an Authorizer panic. Update the call in `runOperation` to pass `oc.Variables`, and the call in `subscription.go`'s open handler to pass the operation's variables (use whichever variable holds them there — read the surrounding code).

In `authz_exec.go`, at the top of `enforceAuth`:

```go
	// A denied argument refuses the field whatever its output outcome is.
	for _, i := range f.argSites {
		if o := st.decision.Outcome(int(i)); o.act == actionDeny {
			st.fieldError(ctx, o.denial(), path, f)
			return true, false
		}
	}
```

In `subscription.go`'s open handler, after the existing root-output Deny check:

```go
		for _, i := range f.argSites {
			if o := d.Outcome(int(i)); o.act == actionDeny {
				return nil, e.subscribeError(ctx, o.denial().WithPath(Path{{Key: f.alias}}))
			}
		}
```

Update the open handler's comment so the list of things that refuse the stream includes a denied argument.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race -run 'TestArgumentSite|TestArgumentDeny|TestInputKeys|TestAuthorizer|TestSubscribe|TestOutcome|TestTypename' .`
Expected: PASS.

- [ ] **Step 5: Prove it can fail**

(a) Remove the argument loop from `enforceAuth`. The query denial cases must FAIL. Revert.
(b) Remove the argument loop from the open handler. The subscription test must FAIL. Revert.
(c) Remove the `hasArgSites` block from `runAuthorizer`. `TestArgumentSiteInputIsReportedOnlyWhenPlanHasArgumentSites` must FAIL. Revert.

- [ ] **Step 6: Gate and commit**

Run: `go test -race -count=1 . ./transport/...` and `sh scripts/gate.sh -short`.

```bash
git add authz.go exec.go authz_exec.go subscription.go authz_input_test.go
git commit -m "feat: show Authorizers supplied argument input and enforce its denial

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Performance gate

`authorize` gained a parameter and `enforceAuth` a loop. Neither should touch a plan without argument sites, and fields without sites never reach `enforceAuth`.

**Files:** none committed unless a benchmark is added.

- [ ] **Step 1: Interleave against the base**

The controller builds `before.exe` from the plan's base commit and places it at `.superpowers/sdd/2026-09-18-authz-2b-argument-sites/before.exe`. Do not create worktrees.

```bash
WS=.superpowers/sdd/2026-09-18-authz-2b-argument-sites
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

**Acceptance:** no statistically significant regression on any of the three, allocs/op equal, `execState` 64 and `OperationContext` 160. If a regression appears, report the table and a diagnosis; do not tune.

---

### Task 6: Documentation

**Files:**
- Modify: `doc.go` (authorization section), `CLAUDE.md` ("Authorization is compiled, not wrapped." paragraph only), the spec's Status line and a Deviations note at the end of §9.4.

- [ ] **Step 1: `doc.go`** — replace the sentence saying `SiteFilterArg` and `SiteInputWrite` are not yet populated with a description of `@authorizeInput`, what `Decision.Input` reports (key paths, enum values, explicit nulls; never scalar values; only what the client supplied, an operation variable default counting and an SDL default not), that only `Allow` and `Deny` apply and `Deny` refuses the field before its resolver, that `ScopeAuthorizer` leaves argument sites allowed, and that `RequireAuthCoverage` does not require argument declarations. Keep `Drop()` described as not implemented.
- [ ] **Step 2: `CLAUDE.md`** — add one or two sentences to that paragraph: argument sites route through the existing `authIdx` compare by giving the field a zero-requirement output site, so a field with none pays nothing; and the input walk reads the AST plus variables, never `ArgumentMap`, because `ArgumentMap` fills in SDL defaults the client did not choose.
- [ ] **Step 3: Spec** — Status says 2b is implemented on `feat/authz-argument-sites`; Deviations lists anything that shipped beyond §9.4.
- [ ] **Step 4:** verify each changed sentence against code, run `go vet ./... && sh scripts/gate.sh -short`, commit with a `docs:` message.

---

## Self-Review Notes

- **§9.4 coverage:** D1 (Task 2), D2 (Task 3), D3 (Task 1 walk, Task 4 reporting), D4 (Task 4 validFor + enforceAuth), D5 (Task 3 routing, Task 4 enforceAuth), D6 (Task 4 open handler), D7 (Task 3 `hasArgSites`, Task 4 gate), D8 (no code: argument sites have a zero requirement; exercised by `TestArgumentDenyAppliesAlongsideAnOutputRequirement` using ScopeAuthorizer), D9 (Task 6 documents it).
- **Flagged for the implementer to verify rather than assumed:** how `NewSchema` binds input-object argument types (Task 3 fixture); `argCoordinate`'s exact format; where the operation's variables live inside the subscription open handler.
- **Fixed during self-review:** Task 1's literal and variable branches disagreed on reporting intermediate keys, and the plan had said only "make them agree" — a placeholder by this plan's own rule. Both branches now carry the same code.
- **Type consistency:** `InputKey{Path, Enum, Null}`, `inputKeys(types, t, v, vars)`, `Decision.Input(site)`, `AuthSite.Arg`, `argType`, `argValue`, `hasArgSites`, `planField.argSites`, `authorize(ctx, p, vars)` are used identically across tasks.
