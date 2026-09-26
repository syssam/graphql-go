package graphql

import (
	"context"
	"testing"
)

// The cost guard prices a list by its page argument, and that argument arrives
// as whatever the variables decoder produced -- json.Number or float64 over
// HTTP, int64 for a literal in the query text. asCostInt is the one place that
// reconciles them, and a branch it gets wrong does not fail: it returns
// "not a number", the field silently falls back to DefaultListSize, and a
// query asking for a page of a million is priced as one of ten. So the guard
// is tested through the decoder rather than on the function.
func costProbeSchema(t *testing.T) *Schema {
	t.Helper()
	type user struct{ ID ID }
	type pageArgs struct{ First *int }
	s, err := NewSchema(SDL(`type User { id: ID! } type Query { users(first: Int): [User!]! }`),
		Args[pageArgs](),
		Object[user]("User", Field("id", func(u *user) ID { return u.ID })),
		Query(ResolveArgs("users", func(_ context.Context, _ Root, a pageArgs) ([]*user, error) {
			return []*user{{"1"}}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// costReported reads the cost the guard computed off its own rejection.
// Saturation is TestQueryCostSaturatesInsteadOfWrapping's; this file is about
// which Go type the page size arrived as.
func costReported(t *testing.T, s *Schema, query, vars string) int {
	t.Helper()
	e := NewExecutor(s, WithQueryCost(QueryCost{Max: 1, DefaultListSize: 10}))
	resp := run(t, e, query, vars)
	if !resp.HasRequestErrors() {
		t.Fatalf("query was not rejected, so no cost was reported: %s", resp.Data)
	}
	n, ok := resp.Errors[0].Extensions["requestedQueryCost"].(int)
	if !ok {
		t.Fatalf("no requestedQueryCost: %s", errorsJSON(resp.Errors))
	}
	return n
}

func TestPageSizeCostsTheSameAsALiteralAndAsAVariable(t *testing.T) {
	s := costProbeSchema(t)
	// 1 + 1*500 = 501, whichever way the 500 arrived.
	literal := costReported(t, s, `{ users(first: 500) { id } }`, "")
	if literal != 501 {
		t.Fatalf("literal cost = %d, want 501", literal)
	}
	variable := costReported(t, s, `query($n: Int){ users(first: $n) { id } }`, `{"n":500}`)
	if variable != literal {
		t.Errorf("a page size of 500 costs %d as a variable and %d as a literal; "+
			"the variable fell back to DefaultListSize, so the guard under-prices every "+
			"paginated query a client sends with variables", variable, literal)
	}
}

// A page size that is not a number at all has to fall back rather than be
// read as zero, which would price the whole subtree at nothing.
func TestANonNumericPageSizeFallsBackToTheDefault(t *testing.T) {
	s := costProbeSchema(t)
	// DefaultListSize 10: 1 + 1*10 = 11.
	if got := costReported(t, s, `{ users { id } }`, ""); got != 11 {
		t.Fatalf("absent first costs %d, want the default 11", got)
	}
	// first: 0 asks for none and is not a positive page size, so it is the
	// same fallback rather than a free query.
	if got := costReported(t, s, `{ users(first: 0) { id } }`, ""); got != 11 {
		t.Errorf("first: 0 costs %d, want the default 11", got)
	}
}

// Int accepts an integral number written as a float or with an exponent
// (rawInt64), so the price has to read the same spellings: one it cannot read
// falls back to DefaultListSize while the resolver still receives the page
// size the client asked for, and Max no longer bounds anything.
func TestAFloatSpelledPageSizeCostsTheSameAsAnInteger(t *testing.T) {
	s := costProbeSchema(t)
	const q = `query($n: Int){ users(first: $n) { id } }`
	for _, vars := range []string{`{"n":500.0}`, `{"n":5e2}`, `{"n":5.0E2}`} {
		if got := costReported(t, s, q, vars); got != 501 {
			t.Errorf("%s costs %d, want 501", vars, got)
		}
	}
	e := NewExecutor(s, WithQueryCost(QueryCost{Max: 100, DefaultListSize: 10}))
	if resp := run(t, e, q, `{"n":2e9}`); !resp.HasRequestErrors() {
		t.Errorf("a page size of 2e9 ran under Max 100: %s", resp.Data)
	}
}
