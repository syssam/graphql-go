package graphql

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
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

func TestNewRequirementCopiesItsGroups(t *testing.T) {
	group := []string{"read"}
	r := NewRequirement(group)
	group[0] = "write"
	if !r.Satisfied(map[string]bool{"read": true}) {
		t.Error("mutating the caller's slice changed the requirement")
	}
	if r.Satisfied(map[string]bool{"write": true}) {
		t.Error("requirement now accepts a scope the caller never declared")
	}
}

// The shape is a property of the plan, not of the caller, so the same
// document yields the same sites regardless of who asks. That is what keeps
// the plan cache from multiplying by policy.
func TestAuthShapeListsOnlyDeclaringFields(t *testing.T) {
	s := shapeSchema(t)
	var shape *AuthShape
	e := NewExecutor(s, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			shape = oc.AuthShape()
			return next(ctx, oc)
		})))
	resp := run(t, e, `{ open me { id salary } }`, "")
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
		t.Fatalf("declaring field a has authIdx %d, want >= 0", byName["a"])
	}
	if byName["b"] != -1 {
		t.Errorf("undeclared field b has authIdx %d, want -1", byName["b"])
	}
	// authIdx alone proves nothing if it points at the wrong site: pin that
	// it actually indexes field a's own site, not merely a non-negative one.
	if got := p.shape.Sites()[byName["a"]].Coord; got != "Query.a" {
		t.Errorf("shape.Sites()[a's authIdx].Coord = %q, want Query.a", got)
	}
}

// The byType branch of shapeBuilder.walk is what gives interface- and
// union-selected fields a site at all; without it every field selected
// through an abstract parent keeps authIdx 0 and Task 6 would enforce
// whatever decision happens to sit at site 0 against it. Pin both that a
// declaring field on one concrete type gets its own correct site and that a
// non-declaring field on a sibling concrete type stays unindexed.
func TestAuthShapeCoversFieldsSelectedThroughAnInterface(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
interface Pet { name: String! }
type Dog implements Pet { name: String! barks: Boolean! @requiresScopes(scopes: [["dog:read"]]) }
type Cat implements Pet { name: String! lives: Int! }
type Query { pet: Pet! }
`
	s, err := NewSchema(SDL(sdl),
		Query(Field("pet", func(Root) shapePet { return &shapeDog{} })),
		Interface[shapePet]("Pet"),
		Object[shapeDog]("Dog",
			Field("name", func(*shapeDog) string { return "" }),
			Field("barks", func(*shapeDog) bool { return true }),
		),
		Object[shapeCat]("Cat",
			Field("name", func(*shapeCat) string { return "" }),
			Field("lives", func(*shapeCat) int { return 9 }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s)
	p, _, perrs := planForTest(t, e, `{ pet { name ... on Dog { barks } ... on Cat { lives } } }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	pet := p.sel.forType(p.root).fields[0]
	if pet.sub == nil || pet.sub.byType == nil {
		t.Fatal("pet should compile as an abstract selection")
	}

	dog := map[string]int32{}
	for _, f := range pet.sub.byType["Dog"].fields {
		dog[f.name] = f.authIdx
	}
	barksIdx := dog["barks"]
	if barksIdx < 0 {
		t.Fatalf("Dog.barks authIdx = %d, want >= 0", barksIdx)
	}
	if got := p.shape.Sites()[barksIdx].Coord; got != "Dog.barks" {
		t.Errorf("site coord = %q, want Dog.barks", got)
	}

	cat := map[string]int32{}
	for _, f := range pet.sub.byType["Cat"].fields {
		cat[f.name] = f.authIdx
	}
	if got := cat["lives"]; got != -1 {
		t.Errorf("Cat.lives authIdx = %d, want -1", got)
	}
}

type shapePet interface{ petName() string }
type shapeDog struct{}
type shapeCat struct{}

func (d *shapeDog) petName() string { return "dog" }
func (c *shapeCat) petName() string { return "cat" }

// NewSchema must reject a "scopes" value that is not a non-empty list of
// non-empty lists of strings: gqlparser checks the directive's name,
// location and required-argument presence, but never the argument value's
// shape, so an unchecked value one nesting level short of Apollo's syntax
// (a flat list of strings) would silently decode to a Requirement satisfied
// by everyone while still creating a site — a field that looks guarded but
// is not.
func TestNewSchemaRejectsMalformedScopesValue(t *testing.T) {
	cases := []struct {
		name   string
		scopes string
	}{
		{"one nesting level short (the commonest Apollo typo)", `["x"]`},
		{"not a list at all", `"x"`},
		{"empty outer list", `[]`},
		{"empty inner group", `[[]]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdl := "directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT\n" +
				"type Query { a: String! @requiresScopes(scopes: " + tc.scopes + ") }\n"
			_, err := NewSchema(SDL(sdl), Query(Field("a", func(Root) string { return "a" })))
			if err == nil {
				t.Fatalf("NewSchema accepted scopes: %s", tc.scopes)
			}
			if !strings.Contains(err.Error(), "Query.a") {
				t.Errorf("error does not name the coordinate Query.a: %v", err)
			}
		})
	}
}

// The same malformed-value check applies to @requiresScopes on an OBJECT
// definition, not only on a field: Plan 2 will read that position too, and
// the validation walks both regardless of which one this task consumes.
func TestNewSchemaRejectsMalformedScopesValueOnObject(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query @requiresScopes(scopes: ["x"]) { a: String! }
`
	_, err := NewSchema(SDL(sdl), Query(Field("a", func(Root) string { return "a" })))
	if err == nil {
		t.Fatal("NewSchema accepted a malformed scopes value on an object type")
	}
	if !strings.Contains(err.Error(), "Query") {
		t.Errorf("error does not name the coordinate Query: %v", err)
	}
}

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

type shapeUser struct {
	ID     string
	Salary int
}

const shapeSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { me: User! open: String! }
type User { id: ID! salary: Int! @requiresScopes(scopes: [["pay:read"]]) }
`

func shapeSchema(t testing.TB) *Schema {
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

func TestAuthorizerRunsOncePerOperation(t *testing.T) {
	var calls atomic.Int64
	s := shapeSchema(t)
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

// This does not use shapeSchema: it needs to count calls to the me
// resolver, which the shared helper has no way to expose without changing
// its signature for every other test that uses it.
func TestAuthorizerErrorRejectsTheOperation(t *testing.T) {
	var meCalls atomic.Int64
	s, err := NewSchema(SDL(shapeSDL),
		Query(
			Resolve("me", func(context.Context, Root) (*shapeUser, error) {
				meCalls.Add(1)
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
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return Errorf("nope").WithCode(CodeForbidden)
		})))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("operation was not rejected")
	}
	if resp.Data != nil && string(resp.Data) != "null" {
		t.Errorf("rejected operation returned data: %s", resp.Data)
	}
	if got, want := resp.Errors[0].Extensions["code"], CodeForbidden; got != want {
		t.Errorf("rejected operation's error code = %v, want %v", got, want)
	}
	if n := meCalls.Load(); n != 0 {
		t.Errorf("me resolver ran %d times, want 0: rejection must happen before any field resolves", n)
	}
}

// A custom ErrorPresenter may log or attach an incident ID; running it twice
// for one rejected operation would double both. requestError must present
// the Authorizer's error exactly once.
func TestAuthorizerRejectionPresentsTheErrorExactlyOnce(t *testing.T) {
	var presented atomic.Int64
	s := shapeSchema(t)
	e := NewExecutor(s,
		WithAuthorizer(AuthorizerFunc(
			func(ctx context.Context, shape *AuthShape, d *Decision) error {
				return Errorf("nope").WithCode(CodeForbidden)
			})),
		WithErrorPresenter(func(ctx context.Context, err error) *Error {
			presented.Add(1)
			return DefaultErrorPresenter(ctx, err)
		}),
	)
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(resp.Errors))
	}
	if n := presented.Load(); n != 1 {
		t.Errorf("ErrorPresenter ran %d times, want 1", n)
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

// ScopeAuthorizer records a Deny for a site whose scopes are not held. What
// the write path then does with that Deny is Task 6; this asserts only that
// the decision was recorded, which is all that exists yet.
func TestScopeAuthorizerRecordsDenyForUnheldScopes(t *testing.T) {
	var recorded Outcome
	s := shapeSchema(t)
	base := ScopeAuthorizer(func(context.Context) map[string]bool {
		return map[string]bool{"other": true}
	})
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			if err := base.Authorize(ctx, shape, d); err != nil {
				return err
			}
			recorded = d.Outcome(0)
			return nil
		})))
	run(t, e, `{ me { salary } }`, "")
	if recorded.act != actionDeny {
		t.Errorf("outcome for an unheld scope = %v, want actionDeny", recorded.act)
	}
	if !strings.Contains(recorded.denial().Message, "or it might not exist") {
		t.Errorf("denial does not use the AIP-211 wording: %s", recorded.denial().Message)
	}
}

// AuthShape.Sites() must not hand back the plan's own backing slice: the
// shape is cached with the plan and reused by every later request, and
// Authorize hands it to user-supplied code. A caller that mutates a
// returned AuthSite must not be able to corrupt authorization for a later
// request that reuses the same plan.
func TestAuthShapeSitesReturnsAnIndependentCopy(t *testing.T) {
	s := shapeSchema(t)
	e := NewExecutor(s)
	p, _, perrs := planForTest(t, e, `{ me { salary } }`)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}

	sites := p.shape.Sites()
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	sites[0].Coord = "corrupted"
	sites[0].Requires = NewRequirement([]string{"corrupted"})

	again := p.shape.Sites()
	if again[0].Coord != "User.salary" {
		t.Errorf("mutating a returned site changed the plan's own site: Coord = %q", again[0].Coord)
	}
	if !again[0].Requires.Satisfied(map[string]bool{"pay:read": true}) {
		t.Error("mutating a returned site's Requires changed the plan's own requirement")
	}

	scopes := p.shape.Scopes()
	scopes[0] = "corrupted"
	if got := p.shape.Scopes(); len(got) != 1 || got[0] != "pay:read" {
		t.Errorf("mutating a returned Scopes() slice changed the plan's own scopes: %v", got)
	}
}

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

// Field interceptors force a field through the type-erased path
// (interceptedExec). Deny/Null/Zero are decided in writeFieldValue before
// either the intercepted or plain executor runs, so a no-op interceptor
// must not change any of these outcomes; Redact must still rewrite the
// resolved value.
func TestOutcomeEnforcementWithFieldInterceptor(t *testing.T) {
	noop := FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
		return next(ctx)
	})
	cases := []struct {
		name    string
		outcome Outcome
		want    string
		wantErr string
	}{
		{"zero replaces a non-null leaf", Zero(), `{"me":{"salary":0}}`, ""},
		{"redact rewrites the value", Redact(func(any) any { return 7 }), `{"me":{"salary":7}}`, ""},
		{"deny reports and bubbles", Deny("pay:read", "User.salary"), "", "or it might not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := shapeSchema(t)
			e := NewExecutor(s,
				WithAuthorizer(AuthorizerFunc(func(ctx context.Context, shape *AuthShape, d *Decision) error {
					return d.Set(0, tc.outcome)
				})),
				WithFieldInterceptor(noop),
			)
			resp := run(t, e, `{ me { salary } }`, "")
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

// A no-op interceptor cannot show whether Redact's resolve actually passed
// through the field interceptor chain; a counting one can. callLeafRedacted
// resolves through f.exec.resolveAny, the plan field's own executor, which
// interceptedExec wraps exactly like writeLeaf -- so a field interceptor
// (ext/otel field spans, audit/masking middleware) must see a redacted field
// exactly once, the same as any other leaf.
func TestOutcomeRedactObservedByFieldInterceptor(t *testing.T) {
	var seen atomic.Int64
	counting := FieldInterceptorFunc(func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
		if fc.Field.Name == "salary" {
			seen.Add(1)
		}
		return next(ctx)
	})
	s := shapeSchema(t)
	e := NewExecutor(s,
		WithAuthorizer(AuthorizerFunc(func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Redact(func(any) any { return 7 }))
		})),
		WithFieldInterceptor(counting),
	)
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"me":{"salary":7}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("field interceptor observed the redacted field %d times, want 1", n)
	}
}

// A subscription's per-event root field substitutes pf.exec, not
// fd.anyResolve (runSubscriptionEvent): fd.anyResolve on a Subscribe-bound
// field is permanently the errSubscriptionResolved stub. callLeafRedacted
// must resolve through f.exec.resolveAny to see the substituted event
// rather than that stub.
func TestOutcomeRedactOnSubscriptionRootField(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { ping: String! }
type Subscription { tick: Int! @requiresScopes(scopes: [["x"]]) }
`
	ch := make(chan int)
	s, err := NewSchema(SDL(sdl),
		Query(Field("ping", func(Root) string { return "pong" })),
		Subscription(Subscribe("tick", func(context.Context) (<-chan int, error) { return ch, nil })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Redact(func(any) any { return 99 }))
		})))

	respCh, err := e.Subscribe(context.Background(), &Request{Query: `subscription { tick }`})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ch <- 1
	resp := <-respCh
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"tick":99}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

// Zero and Deny apply through Outcome.validFor, not ad hoc bubbling logic:
// a literal null for a non-null field is not a value the schema allows, so
// Null on a non-null site must be refused at Decision.Set rather than
// silently written and left to the caller to notice.
func TestNullOutcomeRejectedOnNonNullField(t *testing.T) {
	s := shapeSchema(t)
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Null())
		})))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("operation was not rejected")
	}
	if !strings.Contains(resp.Errors[0].Message, "Null is not valid for") {
		t.Errorf("error = %s, want it to name Null as invalid", resp.Errors[0].Message)
	}
}

// enforceAuth's actionNull case is the only thing that makes Null() do
// anything: deleting it from the switch leaves every other enforcement test
// green (Deny, Zero and Redact all have their own cases) while Null()
// silently falls through to the ordinary resolve path. This is that case's
// own coverage, on a nullable leaf, asserting both the written value and
// that the resolver underneath never ran.
func TestNullOutcomeAppliesToNullableLeaf(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { leaf: Int @requiresScopes(scopes: [["x"]]) }
`
	var calls atomic.Int64
	s, err := NewSchema(SDL(sdl),
		Query(Resolve("leaf", func(context.Context, Root) (int, error) {
			calls.Add(1)
			return 42, nil
		})),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Null())
		})))
	resp := run(t, e, `{ leaf }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"leaf":null}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("resolver ran %d times, want 0", n)
	}
}

// Same as above, for a nullable composite: Null on an object-typed field
// must not resolve the object (and so not resolve anything beneath it)
// either.
func TestNullOutcomeAppliesToNullableComposite(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { obj: Nested @requiresScopes(scopes: [["x"]]) }
type Nested { id: ID! }
`
	type nested struct{ ID string }
	var calls atomic.Int64
	s, err := NewSchema(SDL(sdl),
		Query(Resolve("obj", func(context.Context, Root) (*nested, error) {
			calls.Add(1)
			return &nested{ID: "1"}, nil
		})),
		Object[nested]("Nested",
			Field("id", func(n *nested) ID { return ID(n.ID) }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Null())
		})))
	resp := run(t, e, `{ obj { id } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"obj":null}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("resolver ran %d times, want 0", n)
	}
}

// A Redact built with a nil function passes every other validFor check
// (leaf, not a list) and would only panic at write time, on whichever
// request first reaches it. Reject it at Decision.Set instead, by name.
func TestRedactNilFunctionRejected(t *testing.T) {
	s := shapeSchema(t)
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Redact(nil))
		})))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("operation was not rejected")
	}
	msg := resp.Errors[0].Message
	if !strings.Contains(msg, "User.salary") || !strings.Contains(msg, "nil function") {
		t.Errorf("error = %s, want it to name the coordinate and a nil function", msg)
	}
}

// Zero's rejection message offers Null as an alternative only when Null
// would itself be valid there -- a non-null field can take neither, so
// naming Null as an option would send the caller straight into a second
// rejection.
func TestZeroRejectionSuggestsNullOnlyWhenNullable(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
enum Role { ADMIN USER }
type Query {
  roleNullable: Role @requiresScopes(scopes: [["x"]])
  roleRequired: Role! @requiresScopes(scopes: [["x"]])
}
`
	s, err := NewSchema(SDL(sdl),
		Enum("Role", roleNames),
		Query(
			Field("roleNullable", func(Root) *role { r := roleAdmin; return &r }),
			Field("roleRequired", func(Root) role { return roleAdmin }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	cases := []struct {
		name  string
		query string
		want  string
		bad   string
	}{
		{"nullable field suggests Null", `{ roleNullable }`, "use Deny or Null", ""},
		{"non-null field suggests only Deny", `{ roleRequired }`, "use Deny", "or Null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					return d.Set(0, Zero())
				})))
			resp := run(t, e, tc.query, "")
			if len(resp.Errors) == 0 {
				t.Fatal("operation was not rejected")
			}
			msg := resp.Errors[0].Message
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %s, want it to contain %q", msg, tc.want)
			}
			if tc.bad != "" && strings.Contains(msg, tc.bad) {
				t.Errorf("error = %s, want it NOT to contain %q", msg, tc.bad)
			}
		})
	}
}

// Only Int was ever exercised for Zero; the other four built-in scalars and
// a list each have their own literal in writeZero and deserve their own
// case rather than an inference from Int's.
func TestZeroWritesEveryBuiltinScalarAndAList(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query {
  str: String! @requiresScopes(scopes: [["x"]])
  id: ID! @requiresScopes(scopes: [["x"]])
  num: Int! @requiresScopes(scopes: [["x"]])
  flt: Float! @requiresScopes(scopes: [["x"]])
  flag: Boolean! @requiresScopes(scopes: [["x"]])
  list: [Int!]! @requiresScopes(scopes: [["x"]])
}
`
	s, err := NewSchema(SDL(sdl),
		Query(
			Field("str", func(Root) string { return "hi" }),
			Field("id", func(Root) ID { return ID("abc") }),
			Field("num", func(Root) int { return 7 }),
			Field("flt", func(Root) float64 { return 3.5 }),
			Field("flag", func(Root) bool { return true }),
			Field("list", func(Root) []int { return []int{1, 2, 3} }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	cases := []struct {
		field string
		want  string
	}{
		{"str", `{"str":""}`},
		{"id", `{"id":""}`},
		{"num", `{"num":0}`},
		{"flt", `{"flt":0}`},
		{"flag", `{"flag":false}`},
		{"list", `{"list":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
				func(ctx context.Context, shape *AuthShape, d *Decision) error {
					return d.Set(0, Zero())
				})))
			resp := run(t, e, "{ "+tc.field+" }", "")
			if len(resp.Errors) > 0 {
				t.Fatalf("errors: %s", errorsJSON(resp.Errors))
			}
			if got := string(resp.Data); got != tc.want {
				t.Errorf("data = %s, want %s", got, tc.want)
			}
		})
	}
}

// Deny on User.salary (Int!, under User!) bubbles all the way to the root:
// the field error alone doesn't show that -- the FORBIDDEN code, the full
// path to the denied coordinate, and the fully-bubbled data are the parts a
// substring match on the message can miss.
func TestOutcomeDenyReportsCodePathAndBubbles(t *testing.T) {
	s := shapeSchema(t)
	e := NewExecutor(s, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Deny("pay:read", "User.salary"))
		})))
	resp := run(t, e, `{ me { salary } }`, "")
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %d, want 1: %s", len(resp.Errors), errorsJSON(resp.Errors))
	}
	got := resp.Errors[0]
	if got.Extensions["code"] != CodeForbidden {
		t.Errorf("code = %v, want %v", got.Extensions["code"], CodeForbidden)
	}
	wantPath := Path{{Key: "me"}, {Key: "salary"}}
	if len(got.Path) != len(wantPath) {
		t.Fatalf("path = %v, want %v", got.Path, wantPath)
	}
	for i := range wantPath {
		if got.Path[i] != wantPath[i] {
			t.Errorf("path[%d] = %+v, want %+v", i, got.Path[i], wantPath[i])
		}
	}
	if got, want := string(resp.Data), "null"; got != want {
		t.Errorf("data = %s, want %s: salary is non-null under a non-null me, so the denial bubbles past both", got, want)
	}
}

// writeFieldsConcurrent (exec_object.go) is a second call site of
// writeFieldValue, taken only when a selection has at least two
// concurrently-schedulable fields. Both fields here are Resolve-bound so
// planField.schedulable is true for both and directSchedulable reaches 2,
// forcing that path; an enforcement point wired only into the serial loop
// would leave one of these two unauthorized.
func TestOutcomeEnforcementOnConcurrentFields(t *testing.T) {
	const sdl = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Query { me: User! }
type User {
  a: Int! @requiresScopes(scopes: [["x"]])
  b: Int! @requiresScopes(scopes: [["x"]])
}
`
	s, err := NewSchema(SDL(sdl),
		Query(Resolve("me", func(context.Context, Root) (*shapeUser, error) { return &shapeUser{Salary: 9}, nil })),
		Object[shapeUser]("User",
			Resolve("a", func(_ context.Context, u *shapeUser) (int, error) { return u.Salary, nil }),
			Resolve("b", func(_ context.Context, u *shapeUser) (int, error) { return u.Salary, nil }),
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
	resp := run(t, e, `{ me { a b } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"me":{"a":0,"b":0}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}

// authSubMessage/authSubSDL/newAuthSubExecutor give the two subscription
// authorization tests below their own minimal, self-contained fixture
// instead of extending subscription_test.go's shared subSDL/subSource: those
// are read by every test in that file, and this task's ruling was explicit
// that an existing subscription test's behaviour must not shift as a side
// effect of this change.
type authSubMessage struct {
	ID     string
	Secret string
}

const authSubSDL = `
directive @requiresScopes(scopes: [[String!]!]!) on FIELD_DEFINITION | OBJECT
type Message { id: ID! secret: String! @requiresScopes(scopes: [["msg:read"]]) }
type Query { ping: String! }
type Subscription { messages: Message! }
`

// authSubSource holds the channel a test feeds events through and counts
// how many times the source was actually opened, mirroring subSource's
// discipline in subscription_test.go: an error return alone does not prove
// the source was never reached.
type authSubSource struct {
	ch      chan *authSubMessage
	opens   atomic.Int64
	openErr error
}

func newAuthSubExecutor(t *testing.T, opts ...ExecutorOption) (*authSubSource, *Executor) {
	t.Helper()
	src := &authSubSource{ch: make(chan *authSubMessage)}
	s, err := NewSchema(SDL(authSubSDL),
		Query(Field("ping", func(Root) string { return "pong" })),
		Object[authSubMessage]("Message",
			Field("id", func(m *authSubMessage) ID { return ID(m.ID) }),
			Field("secret", func(m *authSubMessage) string { return m.Secret }),
		),
		Subscription(
			Subscribe("messages", func(context.Context) (<-chan *authSubMessage, error) {
				src.opens.Add(1)
				if src.openErr != nil {
					return nil, src.openErr
				}
				return src.ch, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return src, NewExecutor(s, opts...)
}

// Spec §5.7 / case #20: each subscription event builds its own
// OperationContext and runs the whole operation chain, so the Authorizer
// must re-run per event and a mid-stream scope revocation must take effect
// on the very next event — without tearing the stream down, since a
// transient policy blip should not disconnect every other event on the
// same subscription.
func TestSubscriptionReauthorizesEveryEvent(t *testing.T) {
	var allow atomic.Bool
	allow.Store(true)
	src, e := newAuthSubExecutor(t, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			if !allow.Load() {
				return Errorf("nope").WithCode(CodeForbidden)
			}
			return nil
		})))

	ch, err := e.Subscribe(context.Background(), &Request{Query: `subscription { messages { id secret } }`})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	src.ch <- &authSubMessage{ID: "1", Secret: "a"}
	resp1 := <-ch
	if len(resp1.Errors) != 0 {
		t.Fatalf("first event errored: %s", errorsJSON(resp1.Errors))
	}

	allow.Store(false)
	src.ch <- &authSubMessage{ID: "2", Secret: "b"}
	resp2 := <-ch
	if len(resp2.Errors) == 0 {
		t.Fatal("second event was not rejected after scopes were revoked")
	}
	if resp2.Data != nil && string(resp2.Data) != "null" {
		t.Errorf("rejected event returned data: %s", resp2.Data)
	}

	// The stream must stay open across a rejected event: a later, authorized
	// event still arrives rather than the channel having been closed.
	allow.Store(true)
	src.ch <- &authSubMessage{ID: "3", Secret: "c"}
	resp3 := <-ch
	if len(resp3.Errors) != 0 {
		t.Fatalf("third event errored though scopes were restored: %s", errorsJSON(resp3.Errors))
	}
}

// Case #20's other half: an unauthorized client must not even open the
// source. Per-event re-authorization alone is not enough, because the
// source is opened once, before the first event, by Subscribe itself.
func TestSubscribeAuthorizesBeforeOpeningTheSource(t *testing.T) {
	src, e := newAuthSubExecutor(t, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return Errorf("nope").WithCode(CodeForbidden)
		})))

	_, err := e.Subscribe(context.Background(), &Request{Query: `subscription { messages { id secret } }`})
	if err == nil {
		t.Fatal("Subscribe did not return an error")
	}
	var serr *SubscribeError
	if !errors.As(err, &serr) {
		t.Fatalf("error is not a *SubscribeError: %v", err)
	}
	if len(serr.Response.Errors) == 0 || serr.Response.Errors[0].Extensions["code"] != CodeForbidden {
		t.Errorf("SubscribeError does not carry the FORBIDDEN code: %+v", serr.Response.Errors)
	}
	if n := src.opens.Load(); n != 0 {
		t.Errorf("source opened %d times, want 0: an unauthorized client must never reach it", n)
	}
}

// runSubscriptionEvent builds its own execState per event (subscription.go);
// a decision that reached only runOperation's execState would authorize the
// first event and leave every later one unenforced. Zero on the event
// payload's declaring field is the write-path assertion; deny is covered by
// the two tests above, which already exercise the reject-the-event path.
func TestOutcomeEnforcementThroughSubscriptionEvent(t *testing.T) {
	src, e := newAuthSubExecutor(t, WithAuthorizer(AuthorizerFunc(
		func(ctx context.Context, shape *AuthShape, d *Decision) error {
			return d.Set(0, Zero())
		})))

	ch, err := e.Subscribe(context.Background(), &Request{Query: `subscription { messages { id secret } }`})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	src.ch <- &authSubMessage{ID: "1", Secret: "top-secret"}
	resp := <-ch
	if len(resp.Errors) != 0 {
		t.Fatalf("event errored: %s", errorsJSON(resp.Errors))
	}
	if got, want := string(resp.Data), `{"messages":{"id":"1","secret":""}}`; got != want {
		t.Errorf("data = %s, want %s", got, want)
	}
}
