package graphql

import (
	"context"
	"testing"
)

func TestMaxComplexityRejects(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxComplexity(3))
	resp := run(t, e, `{ me { id name friends { id } } }`, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeTooComplex {
		t.Fatalf("expected complexity rejection, got %s", errorsJSON(resp.Errors))
	}
}

func TestMaxComplexityAllows(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxComplexity(100))
	resp := run(t, e, `{ me { id } }`, "")
	expectData(t, resp, `{"me":{"id":"1"}}`)
}

func TestMaxDepthRejects(t *testing.T) {
	_, e := newFixtureExecutor(t, WithMaxDepth(2))
	resp := run(t, e, `{ me { friends { id } } }`, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeMaxDepth {
		t.Fatalf("expected depth rejection, got %s", errorsJSON(resp.Errors))
	}
}

func TestQueryCostListMultiplier(t *testing.T) {
	type user struct{ ID ID }
	type pageArgs struct{ First *int }
	s, err := NewSchema(SDL(`type User { id: ID! } type Query { users(first: Int): [User!]! }`),
		Args[pageArgs](),
		Object[user]("User", Field("id", func(u *user) ID { return u.ID })),
		Query(ResolveArgs("users", func(_ context.Context, _ Root, a pageArgs) ([]*user, error) {
			return []*user{{"1"}, {"2"}}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	// users(first: 5) { id } → 1 + 1*5 = 6
	e := NewExecutor(s, WithQueryCost(QueryCost{Max: 5, DefaultListSize: 10}))
	resp := run(t, e, `{ users(first: 5) { id } }`, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["code"] != CodeTooComplex {
		t.Fatalf("expected cost rejection, got %s", errorsJSON(resp.Errors))
	}
	if resp.Errors[0].Extensions["requestedQueryCost"] != 6 {
		t.Fatalf("requestedQueryCost = %v, want 6", resp.Errors[0].Extensions["requestedQueryCost"])
	}

	e = NewExecutor(s, WithQueryCost(QueryCost{Max: 6}))
	expectData(t, run(t, e, `{ users(first: 5) { id } }`, ""), `{"users":[{"id":"1"},{"id":"2"}]}`)
}

func TestQueryCostReportExtension(t *testing.T) {
	_, e := newFixtureExecutor(t, WithQueryCost(QueryCost{Max: 1000, Report: true, DefaultListSize: 1}))
	resp := run(t, e, `{ me { id } }`, "")
	expectData(t, resp, `{"me":{"id":"1"}}`)
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if cost == nil {
		t.Fatalf("missing extensions.cost: %v", resp.Extensions)
	}
	if cost["maxQueryCost"] != 1000 {
		t.Fatalf("maxQueryCost = %v", cost["maxQueryCost"])
	}
}

func TestQueryCostFieldWeight(t *testing.T) {
	s, err := NewSchema(SDL(`type Query { search: String! }`),
		Query(Field("search", func(Root) string { return "ok" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, WithQueryCost(QueryCost{
		Max:         5,
		FieldWeight: map[string]int{"Query.search": 10},
	}))
	resp := run(t, e, `{ search }`, "")
	if !resp.HasRequestErrors() || resp.Errors[0].Extensions["requestedQueryCost"] != 10 {
		t.Fatalf("weighted cost = %s", errorsJSON(resp.Errors))
	}
}

func TestSetExtensionOnOperation(t *testing.T) {
	_, e := newFixtureExecutor(t, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			oc.SetExtension("traceId", "t1")
			return next(ctx, oc)
		},
	)))
	resp := run(t, e, `{ me { id } }`, "")
	expectData(t, resp, `{"me":{"id":"1"}}`)
	if resp.Extensions["traceId"] != "t1" {
		t.Fatalf("extensions = %v", resp.Extensions)
	}
}

type connUser struct{ ID ID }
type connEdge struct {
	Node   *connUser
	Cursor string
}
type connPage struct{ Edges []connEdge }
type connArgs struct{ First, Last *int }

// connCostSchema is Relay-shaped: the pagination arguments sit on the
// connection field, one level above the list they actually bound.
func connCostSchema(t testing.TB) *Schema {
	t.Helper()
	s, err := NewSchema(SDL(`
		type User { id: ID! friends: [User!]! }
		type UserEdge { node: User! cursor: String! }
		type UserConnection { edges: [UserEdge!]! }
		type Query { conn(first: Int, last: Int): UserConnection! }
	`),
		Args[connArgs](),
		Object[connUser]("User",
			Field("id", func(u *connUser) ID { return u.ID }),
			Resolve("friends", func(context.Context, *connUser) ([]*connUser, error) { return []*connUser{}, nil }),
		),
		Object[connEdge]("UserEdge",
			Field("node", func(e *connEdge) *connUser { return e.Node }),
			Field("cursor", func(e *connEdge) string { return e.Cursor }),
		),
		Object[connPage]("UserConnection",
			Field("edges", func(p *connPage) []connEdge { return p.Edges }),
		),
		Query(ResolveArgs("conn", func(context.Context, Root, connArgs) (*connPage, error) {
			return &connPage{Edges: []connEdge{}}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func connCost(t *testing.T, s *Schema, cfg QueryCost, query string) int {
	t.Helper()
	cfg.Report = true
	resp := run(t, NewExecutor(s, WithQueryCost(cfg)), query, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %s", errorsJSON(resp.Errors))
	}
	cost, _ := resp.Extensions["cost"].(map[string]any)
	if cost == nil {
		t.Fatalf("missing extensions.cost: %v", resp.Extensions)
	}
	n, ok := cost["requestedQueryCost"].(int)
	if !ok {
		t.Fatalf("requestedQueryCost = %v", cost["requestedQueryCost"])
	}
	return n
}

// Without the option the page size is invisible, which is the behaviour
// every existing deployment's numbers were set against.
func TestQueryCostIgnoresConnectionArgsByDefault(t *testing.T) {
	s := connCostSchema(t)
	cfg := QueryCost{DefaultListSize: 10}
	small := connCost(t, s, cfg, `{ conn(first: 2) { edges { node { id } } } }`)
	large := connCost(t, s, cfg, `{ conn(first: 200) { edges { node { id } } } }`)
	if small != 22 || large != 22 {
		t.Fatalf("costs = %d and %d, want 22 and 22", small, large)
	}
}

// With it, first on the connection is the page size for the subtree below,
// which is how Shopify and GitHub charge for a connection.
func TestQueryCostConnectionsCountsThePageSize(t *testing.T) {
	s := connCostSchema(t)
	cfg := QueryCost{DefaultListSize: 10, Connections: true}
	if got := connCost(t, s, cfg, `{ conn(first: 2) { edges { node { id } } } }`); got != 7 {
		t.Fatalf("first: 2 cost %d, want 7", got)
	}
	if got := connCost(t, s, cfg, `{ conn(first: 200) { edges { node { id } } } }`); got != 601 {
		t.Fatalf("first: 200 cost %d, want 601", got)
	}
}

// The page size pays for the edges list directly beneath it and nothing
// deeper: a plain list further down still costs DefaultListSize, or the two
// multiply and a modest query prices like a hostile one.
func TestQueryCostConnectionsDoesNotReachPastTheEdges(t *testing.T) {
	s := connCostSchema(t)
	cfg := QueryCost{DefaultListSize: 10, Connections: true}
	if got := connCost(t, s, cfg, `{ conn(first: 5) { edges { node { id friends { id } } } } }`); got != 71 {
		t.Fatalf("cost = %d, want 71", got)
	}
}

// A connection asked for without a page size falls back to the default the
// same way a bare list does.
func TestQueryCostConnectionsWithoutAnArgumentUsesTheDefault(t *testing.T) {
	s := connCostSchema(t)
	cfg := QueryCost{DefaultListSize: 10, Connections: true}
	if got := connCost(t, s, cfg, `{ conn { edges { node { id } } } }`); got != 22 {
		t.Fatalf("cost = %d, want 22", got)
	}
}

// An operation interceptor is where a rate limiter lives, and it wraps the
// engine's own cost check rather than running after it. Cost has to be the
// configured number by then; the default-list-size-1 fallback is for callers
// with no cost model at all, not for one that is merely not computed yet.
func TestCostSeenByAnOperationInterceptorUsesTheConfiguredModel(t *testing.T) {
	var seen int
	e := NewExecutor(connCostSchema(t),
		WithQueryCost(QueryCost{DefaultListSize: 10}),
		WithOperationInterceptor(OperationInterceptorFunc(
			func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
				seen = oc.Cost()
				return next(ctx, oc)
			})))
	run(t, e, `{ conn(first: 2) { edges { node { id } } } }`, "")
	if seen != 22 {
		t.Fatalf("cost seen by the interceptor = %d, want 22 (the configured DefaultListSize)", seen)
	}
}

// The same selection set costs differently paid and unpaid, so anything that
// memoizes this walk must key on paid as well as on the selection set.
//
// Both aliases below select an identical sub-selection on the same type. One
// reaches it through a connection carrying a page size, which pays for the
// edges list beneath it; the other reaches it through the same connection
// field with no page size, where that list takes DefaultListSize as any list
// would. Today those are two distinct *selectionSet values, so the question
// cannot arise and this test cannot fail for that reason — it is here for the
// branch that memoizes compileSelection, where one pointer becomes reachable
// from both parents and a memo keyed on the pointer alone returns whichever
// visit ran first.
func TestQueryCostConnectionsPaidAndUnpaidReachTheSameShape(t *testing.T) {
	cost := connCost(t, connCostSchema(t), QueryCost{DefaultListSize: 10, Connections: true}, `{
		paid: conn(first: 5) { edges { node { id } } }
		free: conn { edges { node { id } } }
	}`)
	// paid: 1 + (1 + 2*1)*5 = 16.  free: 1 + (1 + 2*10) = 22.
	if cost != 38 {
		t.Fatalf("cost = %d, want 38 (16 paid + 22 unpaid); 20 or 128 means one visit's figure was reused for the other", cost)
	}
}

// Interleaved against the same code without the leaf short-circuit, twelve
// alternating rounds of two pre-built binaries: Disabled ~ (p=0.887),
// Reported ~ (p=0.713), Connections -3.76% (p=0.040), allocations equal
// sample for sample. The improvement lands only where it was predicted to,
// on the configuration that was reading arguments off every scalar.
//
// The cost walk runs once per request whenever a cost model is configured,
// and until now nothing measured it. These three are the configurations that
// differ: no model at all, a model that walks, and a model that also reads
// connection arguments. The connection resolver returns an empty page on
// purpose, so what is left is the walk rather than the data.
func benchCost(b *testing.B, cfg *QueryCost) {
	b.Helper()
	var opts []ExecutorOption
	if cfg != nil {
		opts = append(opts, WithQueryCost(*cfg))
	}
	e := NewExecutor(connCostSchema(b), opts...)
	req := &Request{Query: `{ conn(first: 5) { edges { node { id friends { id } } } } }`}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		resp := e.Execute(ctx, req)
		if len(resp.Errors) > 0 {
			b.Fatal(resp.Errors[0])
		}
		resp.Release()
	}
}

func BenchmarkQueryCostDisabled(b *testing.B) { benchCost(b, nil) }

func BenchmarkQueryCostReported(b *testing.B) {
	benchCost(b, &QueryCost{DefaultListSize: 10, Report: true})
}

func BenchmarkQueryCostConnections(b *testing.B) {
	benchCost(b, &QueryCost{DefaultListSize: 10, Report: true, Connections: true})
}

// A request that sends both first and last is priced at the larger page, in
// either argument order. Taking whichever the client named first lets a small
// first hide a large last from the ceiling.
func TestQueryCostConnectionsPricesTheLargerOfFirstAndLast(t *testing.T) {
	s := connCostSchema(t)
	cfg := QueryCost{DefaultListSize: 10, Connections: true}
	want := connCost(t, s, cfg, `{ conn(first: 200) { edges { node { id } } } }`)
	for _, q := range []string{
		`{ conn(first: 2, last: 200) { edges { node { id } } } }`,
		`{ conn(last: 200, first: 2) { edges { node { id } } } }`,
		`{ conn(first: 200, last: 2) { edges { node { id } } } }`,
	} {
		if got := connCost(t, s, cfg, q); got != want {
			t.Fatalf("%s cost %d, want %d (the larger page)", q, got, want)
		}
	}
}

type pagedBox struct{}

type pagedItem struct{}

type pagedArgs struct{ First *int }

// With Connections on, a page size on a non-list field pays for the list
// directly inside it. When that list names a page size of its own it is not
// the connection's edges but a second page, and its size was ignored: the
// client chose a number and the cost did not move.
func TestQueryCostPricesAListsOwnPageSizeUnderAPaidField(t *testing.T) {
	s, err := NewSchema(SDL(`
		type Item { id: ID! }
		type Box { items(first: Int): [Item!]! }
		type Query { box(first: Int): Box }
	`),
		Args[pagedArgs](),
		Object[pagedItem]("Item", Field("id", func(*pagedItem) ID { return "1" })),
		Object[pagedBox]("Box", FieldArgs("items", func(*pagedBox, pagedArgs) []*pagedItem { return nil })),
		Query(FieldArgs("box", func(Root, pagedArgs) *pagedBox { return &pagedBox{} })),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	cfg := QueryCost{DefaultListSize: 10, Connections: true}
	small := connCost(t, s, cfg, `{ box(first: 2) { items(first: 5) { id } } }`)
	large := connCost(t, s, cfg, `{ box(first: 2) { items(first: 500) { id } } }`)
	if large <= small || large < 500 {
		t.Fatalf("items(first: 5) costs %d and items(first: 500) costs %d: the list's own page size is not priced", small, large)
	}
	// The connection shape is untouched: edges with no page size of its own
	// is still paid for by the connection's.
	if got := connCost(t, connCostSchema(t), cfg, `{ conn(first: 2) { edges { node { id } } } }`); got != 7 {
		t.Fatalf("a plain connection costs %d, want 7", got)
	}
}
