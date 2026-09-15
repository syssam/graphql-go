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
