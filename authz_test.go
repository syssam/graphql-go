package graphql

import (
	"context"
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
		t.Errorf("declaring field a has authIdx %d, want >= 0", byName["a"])
	}
	if byName["b"] != -1 {
		t.Errorf("undeclared field b has authIdx %d, want -1", byName["b"])
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
