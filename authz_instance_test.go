package graphql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestObjectAuthBatchDefaultsAndOverrides(t *testing.T) {
	_, e := newFixtureExecutor(t)
	if e.objectAuthBatch != 50 {
		t.Fatalf("default batch = %d, want 50", e.objectAuthBatch)
	}
	_, e = newFixtureExecutor(t, WithObjectAuthBatch(7))
	if e.objectAuthBatch != 7 {
		t.Fatalf("batch = %d, want 7", e.objectAuthBatch)
	}
	// A non-positive size is the caller asking for no splitting at all, which
	// would defeat the bound: keep the default rather than silently accepting.
	_, e = newFixtureExecutor(t, WithObjectAuthBatch(0))
	if e.objectAuthBatch != 50 {
		t.Fatalf("batch = %d, want 50 for a non-positive size", e.objectAuthBatch)
	}
}

const authzObjectSDL = `
directive @authorizeObject on OBJECT
type Customer @authorizeObject { id: ID! name: String! }
type Query { customers: [Customer!]! }
`

// The valid cases below declare @authorizeObject with a wider location list
// than D1's `on OBJECT` alone: with only OBJECT declared, gqlparser's own
// location check rejects the bad placements before validateObjectDirectives
// ever runs, and the test would prove nothing about this validator.
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
			// A root object is written by writeObject directly and never
			// reaches writeValue, so an ObjectAuthorizer is never consulted
			// for it. Accepting the marker here is silence where the author
			// expects enforcement, which is what this validation exists to
			// prevent everywhere else.
			name:    "on a root operation type",
			sdl:     "directive @authorizeObject on OBJECT\ntype Query @authorizeObject { c: String }",
			wantErr: "Query: @authorizeObject is valid only on an object type, not a root operation type",
		},
		{
			name:    "on a root operation type named by the schema block",
			sdl:     "directive @authorizeObject on OBJECT\nschema { query: Root }\ntype Root @authorizeObject { c: String }",
			wantErr: "Root: @authorizeObject is valid only on an object type, not a root operation type",
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
			_, err := NewSchema(SDL(c.sdl))
			if c.wantErr == "" {
				if err != nil && strings.Contains(err.Error(), "authorizeObject") {
					t.Fatalf("valid placement rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

type authzInstanceCustomer struct {
	ID   string
	Name string
}

func TestAuthorizeObjectMarksTheType(t *testing.T) {
	s, err := NewSchema(SDL(authzObjectSDL),
		Object[authzInstanceCustomer]("Customer",
			Field("id", func(*authzInstanceCustomer) ID { return "" }),
			Field("name", func(*authzInstanceCustomer) string { return "" }),
		),
		Query(Field("customers", func(Root) []authzInstanceCustomer { return nil })),
	)
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
		if !strings.Contains(err.Error(), "Customer") {
			t.Fatalf("%s error does not name the coordinate: %v", name, err)
		}
		// The pre-existing Zero/Redact cases in validFor's switch also reject
		// a nil Field, which every instance site has, so a substring check on
		// the coordinate alone would pass even without the SiteInstance rule.
		// Requiring "instance site" in the message pins the rejection to that
		// rule rather than to the coincidence.
		if !strings.Contains(err.Error(), "instance site") {
			t.Fatalf("%s error does not identify the site as an instance site: %v", name, err)
		}
	}
}

const instanceSDL = `
directive @authorizeObject on OBJECT
directive @authorizeInput(kind: AuthorizeInputKind!) on ARGUMENT_DEFINITION
enum AuthorizeInputKind { FILTER WRITE }
input Where { nameContains: String }
interface Node { id: ID! }
type Customer implements Node @authorizeObject { id: ID! name: String! owner: String! manager: Customer }
type Open implements Node { id: ID! }
type Query {
  customers: [Customer!]!
  maybe: Customer
  required: Customer!
  filtered(where: Where @authorizeInput(kind: FILTER)): [Customer!]!
  node: Node
  plain: String!
}
`

type instanceOpen struct{ ID string }

type instanceWhereIn struct{ NameContains *string }

type instanceFilteredArgs struct{ Where *instanceWhereIn }

var (
	instanceC1 = authzInstanceCustomer{ID: "c1", Name: "one"}
	instanceC2 = authzInstanceCustomer{ID: "c2", Name: "two"}
	instanceC3 = authzInstanceCustomer{ID: "c3", Name: "three"}
)

// newInstanceExecutor builds an executor over instanceSDL. Node is left
// unbound, as fixtureSDL's is: an interface whose implementers are distinct Go
// types resolves its concrete type from the dynamic value alone.
func newInstanceExecutor(t *testing.T, opts ...ExecutorOption) *Executor {
	t.Helper()
	return newInstanceExecutorNode(t, func(context.Context, Root) (any, error) {
		return &instanceC1, nil
	}, opts...)
}

func newInstanceExecutorWith(t *testing.T, a ObjectAuthorizer, opts ...ExecutorOption) *Executor {
	t.Helper()
	opts = append([]ExecutorOption{WithObjectAuthorizer(a)}, opts...)
	return newInstanceExecutor(t, opts...)
}

func newInstanceExecutorOpenNode(t *testing.T, a ObjectAuthorizer) *Executor {
	t.Helper()
	return newInstanceExecutorNode(t, func(context.Context, Root) (any, error) {
		return &instanceOpen{ID: "o1"}, nil
	}, WithObjectAuthorizer(a))
}

func newInstanceExecutorNode(t *testing.T, node func(context.Context, Root) (any, error), opts ...ExecutorOption) *Executor {
	t.Helper()
	s, err := NewSchema(SDL(instanceSDL),
		Input[instanceWhereIn]("Where",
			InputField("nameContains", func(w *instanceWhereIn, v *string) { w.NameContains = v }),
		),
		Args[instanceFilteredArgs](
			InputField("where", func(a *instanceFilteredArgs, v *instanceWhereIn) { a.Where = v }),
		),
		Object[authzInstanceCustomer]("Customer",
			Field("id", func(c *authzInstanceCustomer) ID { return ID(c.ID) }),
			Field("name", func(c *authzInstanceCustomer) string { return c.Name }),
			// A resolver field, so a selection that includes it is deeply
			// schedulable and the list takes the concurrent path.
			Resolve("owner", func(_ context.Context, c *authzInstanceCustomer) (string, error) {
				return "owner-" + c.ID, nil
			}),
			// A guarded object reached through a field that is not a list:
			// the position whose checks coalescing has to cover.
			Field("manager", func(c *authzInstanceCustomer) *authzInstanceCustomer {
				return &authzInstanceCustomer{ID: "m-" + c.ID, Name: "manager of " + c.Name}
			}),
		),
		Object[instanceOpen]("Open",
			Field("id", func(o *instanceOpen) ID { return ID(o.ID) }),
		),
		Query(
			Field("customers", func(Root) []authzInstanceCustomer {
				return []authzInstanceCustomer{instanceC1, instanceC2, instanceC3}
			}),
			Field("maybe", func(Root) *authzInstanceCustomer { return &instanceC1 }),
			Field("required", func(Root) authzInstanceCustomer { return instanceC1 }),
			FieldArgs("filtered", func(Root, instanceFilteredArgs) []authzInstanceCustomer {
				return []authzInstanceCustomer{instanceC1, instanceC2, instanceC3}
			}),
			Resolve("node", node),
			Field("plain", func(Root) string { return "" }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return NewExecutor(s, opts...)
}

func constantObjectPolicy(o Outcome) ObjectAuthorizer {
	return objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		outs := make([]Outcome, len(checks))
		for i := range outs {
			outs[i] = o
		}
		return outs, nil
	})
}

func idOf(v any) string {
	switch c := v.(type) {
	case *authzInstanceCustomer:
		return c.ID
	case authzInstanceCustomer:
		return c.ID
	}
	return ""
}

func assertJSON(t *testing.T, data []byte, want string) {
	t.Helper()
	if got := string(data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

func assertErrorContains(t *testing.T, errs []*Error, want string) {
	t.Helper()
	if want == "" {
		if len(errs) != 0 {
			t.Fatalf("errors = %s, want none", errorsJSON(errs))
		}
		return
	}
	for _, e := range errs {
		if e != nil && strings.Contains(e.Message, want) {
			return
		}
	}
	t.Fatalf("errors = %s, want one containing %q", errorsJSON(errs), want)
}

// fieldByName finds a root planField by its response key.
func fieldByName(t *testing.T, p *plan, key string) *planField {
	t.Helper()
	for _, f := range p.sel.forType(p.root).fields {
		if f.alias == key {
			return f
		}
	}
	t.Fatalf("no root field %q in plan", key)
	return nil
}

func planFor(t *testing.T, e *Executor, query string) *plan {
	t.Helper()
	p, _, perrs := planForTest(t, e, query)
	if perrs != nil {
		t.Fatalf("plan: %v", perrs)
	}
	return p
}

// instanceSiteIn finds the one instance site for a type in a shape, the way a
// caller reading AuthShape.Sites() would. The executor does not index it: it
// routes on planField.hasInstanceSite, so there is no derived index to assert.
func instanceSiteIn(t *testing.T, p *plan, coord string) AuthSite {
	t.Helper()
	var found []AuthSite
	for _, s := range p.shape.Sites() {
		if s.Kind == SiteInstance && s.Coord == coord {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d instance sites for %q, want 1", len(found), coord)
	}
	return found[0]
}

func TestInstanceSitesAtPlanCompile(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ customers { id } plain }`)

	f := fieldByName(t, p, "customers")
	if !f.hasInstanceSite() {
		t.Fatal("customers has no instance site")
	}
	if f.argSiteCount() != 0 {
		t.Fatalf("argSiteCount = %d, want 0", f.argSiteCount())
	}
	// A field that declares nothing of its own gets no output site for an
	// instance check: the executor never reaches one through authIdx.
	if f.authIdx != -1 {
		t.Fatalf("authIdx = %d, want -1: an instance check is not routed through enforceAuth", f.authIdx)
	}
	site := instanceSiteIn(t, p, "Customer")
	if !site.Requires.IsZero() {
		t.Fatal("an instance site carries no requirement of its own")
	}
	if site.Field != nil {
		t.Fatal("an instance site has no field of its own")
	}

	plain := fieldByName(t, p, "plain")
	if plain.authIdx != -1 || plain.hasInstanceSite() {
		t.Fatalf("plain: authIdx=%d hasInstanceSite=%v, want -1/false", plain.authIdx, plain.hasInstanceSite())
	}
	// Nothing here declares a requirement, so there is nothing to ask an
	// Authorizer about even though the shape is not empty.
	if !p.shape.IsEmpty() {
		t.Fatal("a shape whose only sites are instance sites is empty to an Authorizer")
	}
}

func TestInstanceSiteAlongsideAnArgumentSite(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ filtered(where: {nameContains: "a"}) { id } }`)
	f := fieldByName(t, p, "filtered")
	if f.argSiteCount() != 1 || !f.hasInstanceSite() {
		t.Fatalf("argSiteCount=%d hasInstanceSite=%v, want 1/true", f.argSiteCount(), f.hasInstanceSite())
	}
	// The argument sites still follow the output site contiguously; enforceAuth
	// walks them by offset and the instance site must not be among them.
	for i := f.authIdx + 1; i <= f.authIdx+f.argSiteCount(); i++ {
		if k := p.shape.sites[i].Kind; k != SiteFilterArg && k != SiteInputWrite {
			t.Fatalf("site %d in the argument range is %v", i, k)
		}
	}
	instanceSiteIn(t, p, "Customer")
	if p.shape.IsEmpty() {
		t.Fatal("an argument site must still reach the Authorizer")
	}
}

func TestInstanceSiteOnAnAbstractPosition(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ node { __typename } }`)
	f := fieldByName(t, p, "node")
	if !f.hasInstanceSite() {
		t.Fatal("an abstract position with a guarded implementer needs an instance site")
	}
	// The abstract type's name, not a concrete implementer's: the concrete
	// type is known only once the value resolves, and ObjectCheck.Type carries
	// it then.
	instanceSiteIn(t, p, "Node")
}

func TestPlanWithoutInstanceSitesIsUnchanged(t *testing.T) {
	e := newInstanceExecutor(t)
	p := planFor(t, e, `{ plain }`)
	if p.shape != nil {
		t.Fatalf("shape = %v, want nil for a plan that touches nothing guarded", p.shape)
	}
}

// indexOf is strings.Index under a name that reads as a boolean-ish check at
// the call site: err.Error() does not contain a fixed substring at a fixed
// position, only somewhere or nowhere.
func indexOf(s, substr string) int { return strings.Index(s, substr) }

// newInstanceState builds an executor over instanceSDL with an
// ObjectAuthorizer installed and returns an execState for it, the way
// runOperation builds one -- checkObjects needs nothing from a live request.
func newInstanceState(t *testing.T, a ObjectAuthorizer, opts ...ExecutorOption) *execState {
	t.Helper()
	opts = append([]ExecutorOption{WithObjectAuthorizer(a)}, opts...)
	e := newInstanceExecutor(t, opts...)
	return &execState{e: e}
}

type objectAuthorizerFunc func(ctx context.Context, checks []ObjectCheck) ([]Outcome, error)

func (f objectAuthorizerFunc) AuthorizeObjects(ctx context.Context, checks []ObjectCheck) ([]Outcome, error) {
	return f(ctx, checks)
}

// instanceChecks builds n checks at one instance site. The site rides on the
// check now, because a coalesced batch can carry several.
func instanceChecks(n int) []ObjectCheck {
	site := AuthSite{Coord: "Customer", Kind: SiteInstance}
	checks := make([]ObjectCheck, n)
	for i := range checks {
		checks[i] = ObjectCheck{Site: site, Type: "Customer"}
	}
	return checks
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
	checks := instanceChecks(5)
	outs, err := st.checkObjects(context.Background(), checks)
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
	_, err := st.checkObjects(context.Background(), instanceChecks(3))
	if err == nil {
		t.Fatal("a short result was accepted, which would silently allow the rows it does not cover")
	}
}

func TestCheckObjectsRejectsAnInvalidOutcome(t *testing.T) {
	a := objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		return []Outcome{Zero()}, nil
	})
	st := newInstanceState(t, a)
	_, err := st.checkObjects(context.Background(), instanceChecks(1))
	if err == nil || indexOf(err.Error(), "instance site") < 0 {
		t.Fatalf("err = %v, want the validFor rejection", err)
	}
}

func TestCheckObjectsHidesABackendFailure(t *testing.T) {
	a := objectAuthorizerFunc(func(_ context.Context, _ []ObjectCheck) ([]Outcome, error) {
		return nil, errors.New("dial tcp 10.0.3.7:8181: connection refused")
	})
	st := newInstanceState(t, a)
	_, err := st.checkObjects(context.Background(), instanceChecks(1))
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
	_, err := st.checkObjects(context.Background(), instanceChecks(1))
	if err == nil {
		t.Fatal("a panic in the ObjectAuthorizer was not recovered")
	}
	if indexOf(err.Error(), "policy exploded") >= 0 {
		t.Fatalf("the panic value reached the client: %v", err)
	}
}

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
			resp := run(t, e, c.query, "")
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
	resp := run(t, e, `{ customers { id } }`, "")
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
	resp := run(t, e, `{ customers { id } }`, "")
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", resp.Errors)
	}
	if got := resp.Errors[0].Path.String(); got != "customers[0]" {
		t.Fatalf("path = %q, want customers[0]", got)
	}
}

func TestInstanceChecksAreBatchedPerList(t *testing.T) {
	var calls, seen int
	e := newInstanceExecutorWith(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
		calls++
		seen += len(checks)
		return make([]Outcome, len(checks)), nil
	}))
	run(t, e, `{ customers { id } }`, "")
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
	run(t, e, `{ node { __typename } }`, "")
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
	run(t, e, `{ node { __typename } }`, "")
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 for an unguarded concrete type", calls)
	}
}

func TestNoObjectAuthorizerEnforcesNothing(t *testing.T) {
	e := newInstanceExecutor(t) // no WithObjectAuthorizer
	resp := run(t, e, `{ customers { id } }`, "")
	assertJSON(t, resp.Data, `{"customers":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`)
}

// instanceSubSource is a channel the test writes to, so each event is
// published only after the previous response has been consumed. The policy
// reads a shared event counter; a source that raced ahead would decide the
// next event with the previous counter.
type instanceSubSource struct {
	ch chan []authzInstanceCustomer
}

func sendInstanceBatch(t *testing.T, ch chan<- []authzInstanceCustomer, batch []authzInstanceCustomer) {
	t.Helper()
	select {
	case ch <- batch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending an event")
	}
}

func newInstanceSubExecutor(t *testing.T, a ObjectAuthorizer) (*instanceSubSource, *Executor) {
	t.Helper()
	src := &instanceSubSource{ch: make(chan []authzInstanceCustomer)}
	const sdl = `
directive @authorizeObject on OBJECT
type Customer @authorizeObject { id: ID! name: String! }
type Query { ping: String! }
type Subscription { batches: [Customer!]! }
`
	s, err := NewSchema(SDL(sdl),
		Object[authzInstanceCustomer]("Customer",
			Field("id", func(c *authzInstanceCustomer) ID { return ID(c.ID) }),
			Field("name", func(c *authzInstanceCustomer) string { return c.Name }),
		),
		Query(Field("ping", func(Root) string { return "" })),
		Subscription(
			Subscribe("batches", func(context.Context) (<-chan []authzInstanceCustomer, error) {
				return src.ch, nil
			}),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return src, NewExecutor(s, WithObjectAuthorizer(a))
}

func TestSubscriptionEventInstanceDropAppliesPerEvent(t *testing.T) {
	var event int
	src, e := newInstanceSubExecutor(t, objectAuthorizerFunc(func(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
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
	batch := []authzInstanceCustomer{instanceC1, instanceC2, instanceC3}
	want := []string{
		`{"batches":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`,
		`{"batches":[{"id":"c1"},{"id":"c3"}]}`,
		`{"batches":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`,
	}
	for i, w := range want {
		event = i + 1
		sendInstanceBatch(t, src.ch, batch)
		select {
		case resp, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed after %d events; a dropped row must not end it", i)
			}
			if len(resp.Errors) != 0 {
				t.Fatalf("event %d errors = %v, want none", i+1, resp.Errors)
			}
			assertJSON(t, resp.Data, w)
			resp.Release()
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d", i+1)
		}
	}
}
