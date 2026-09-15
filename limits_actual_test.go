package graphql

import (
	"context"
	"testing"
)

// Actual cost exists because requested cost has to guess. A connection field
// with no first/last is charged DefaultListSize whatever it returns, so the
// two figures together are what say whether that guess is anywhere near the
// truth.

func costExecutor(t *testing.T, c QueryCost) (*fixture, *Executor) {
	t.Helper()
	return newFixtureExecutor(t, WithQueryCost(c))
}

// costOf runs a query and returns the reported cost payload.
func costOf(t *testing.T, e *Executor, query string) map[string]any {
	t.Helper()
	resp := run(t, e, query, "")
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
	raw, ok := resp.Extensions["cost"]
	if !ok {
		t.Fatalf("no cost in extensions: %v", resp.Extensions)
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("cost is %T", raw)
	}
	return payload
}

func costInt(t *testing.T, payload map[string]any, key string) int {
	t.Helper()
	v, ok := payload[key]
	if !ok {
		t.Fatalf("cost has no %s: %v", key, payload)
	}
	n, ok := v.(int)
	if !ok {
		t.Fatalf("%s is %T", key, v)
	}
	return n
}

// TestActualCostCountsResolvedFields is the whole point: users returns three
// entries, so a query over it costs what three cost, not what the assumed
// default list size costs.
func TestActualCostCountsResolvedFields(t *testing.T) {
	_, e := costExecutor(t, QueryCost{Report: true, Actual: true, DefaultListSize: 10})
	payload := costOf(t, e, `{ users { id name } }`)

	// Requested: 1 for users plus 2 per assumed element.
	if got := costInt(t, payload, "requestedQueryCost"); got != 1+2*10 {
		t.Fatalf("requested = %d, want %d", got, 1+2*10)
	}
	// Actual: 1 for users plus 2 for each of the three it really returned.
	if got := costInt(t, payload, "actualQueryCost"); got != 1+2*3 {
		t.Fatalf("actual = %d, want %d", got, 1+2*3)
	}
}

// TestActualCostMatchesRequestedWhenTheGuessIsRight closes the loop: when the
// list size is known from an argument, the two figures agree, which is what
// makes a divergence meaningful.
func TestActualCostMatchesRequestedWhenTheGuessIsRight(t *testing.T) {
	_, e := costExecutor(t, QueryCost{Report: true, Actual: true})
	payload := costOf(t, e, `{ me { id friends { id } } }`)

	requested := costInt(t, payload, "requestedQueryCost")
	actual := costInt(t, payload, "actualQueryCost")
	// me(1) + id(1) + friends(1) + one id per friend. The fixture gives Ada
	// two friends, and requested assumes DefaultListSize, so they differ by
	// exactly the gap between the guess and the truth.
	if actual != 1+1+1+2 {
		t.Fatalf("actual = %d, want 5", actual)
	}
	if requested <= actual {
		t.Fatalf("requested %d should exceed actual %d when the default list size overshoots", requested, actual)
	}
}

func TestActualCostHonoursFieldWeights(t *testing.T) {
	_, e := costExecutor(t, QueryCost{
		Report: true, Actual: true,
		FieldWeight: map[string]int{"Query.users": 5},
	})
	payload := costOf(t, e, `{ users { id } }`)
	// users is weighted 5, then one id for each of the three entries.
	if got := costInt(t, payload, "actualQueryCost"); got != 5+3 {
		t.Fatalf("actual = %d, want 8", got)
	}
}

func TestActualCostIgnoresTypename(t *testing.T) {
	_, e := costExecutor(t, QueryCost{Report: true, Actual: true})
	with := costInt(t, costOf(t, e, `{ me { __typename id } }`), "actualQueryCost")
	without := costInt(t, costOf(t, e, `{ me { id } }`), "actualQueryCost")
	if with != without {
		t.Fatalf("__typename changed the cost: %d vs %d", with, without)
	}
}

// TestActualCostIsAbsentWhenNotEnabled guards the hot path: without Actual no
// field carries a weight, so nothing is summed and nothing is reported.
func TestActualCostIsAbsentWhenNotEnabled(t *testing.T) {
	_, e := costExecutor(t, QueryCost{Report: true})
	payload := costOf(t, e, `{ users { id } }`)
	if _, ok := payload["actualQueryCost"]; ok {
		t.Fatalf("actual cost reported without being enabled: %v", payload)
	}
	if _, ok := payload["requestedQueryCost"]; !ok {
		t.Fatalf("requested cost should still be reported: %v", payload)
	}
}

// TestRejectedOperationReportsNoActualCost is the distinction a plain zero
// would erase: an operation stopped by the cap resolved nothing, which is not
// the same as having been free.
func TestRejectedOperationReportsNoActualCost(t *testing.T) {
	_, e := newFixtureExecutor(t, WithQueryCost(QueryCost{
		Report: true, Actual: true, Max: 3,
	}))
	resp := run(t, e, `{ users { id name } }`, "")
	defer resp.Release()
	if !resp.HasRequestErrors() {
		t.Fatalf("wanted a rejection, got %s", resp.Data)
	}
	payload, ok := resp.Extensions["cost"].(map[string]any)
	if !ok {
		t.Fatalf("no cost payload: %v", resp.Extensions)
	}
	if _, ok := payload["actualQueryCost"]; ok {
		t.Fatalf("a rejected operation resolved nothing and must not report an actual cost: %v", payload)
	}
	if got := costInt(t, payload, "maxQueryCost"); got != 3 {
		t.Fatalf("max = %d", got)
	}
}

// TestActualCostSurvivesConcurrentFields checks the counter under the
// scheduling it will actually meet: sibling resolvers run on their own
// goroutines, so the sum has to be atomic.
func TestActualCostSurvivesConcurrentFields(t *testing.T) {
	_, e := costExecutor(t, QueryCost{Report: true, Actual: true})
	const query = `{ a: users { id } b: users { id } c: users { id } }`

	want := costInt(t, costOf(t, e, query), "actualQueryCost")
	if want != 3*(1+3) {
		t.Fatalf("actual = %d, want 12", want)
	}
	for range 20 {
		if got := costInt(t, costOf(t, e, query), "actualQueryCost"); got != want {
			t.Fatalf("actual cost varied between runs: %d then %d", want, got)
		}
	}
}

// TestActualCostIsAvailableFromTheOperationContext covers the path that does
// not go through extensions: a metrics extension wants the number without
// putting it in every client's response.
func TestActualCostIsAvailableFromTheOperationContext(t *testing.T) {
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), f.options()...)
	if err != nil {
		t.Fatal(err)
	}

	var measured int
	var measuredOK bool
	e := NewExecutor(s,
		WithQueryCost(QueryCost{Actual: true}),
		WithOperationInterceptor(OperationInterceptorFunc(
			func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
				resp := next(ctx, oc)
				measured, measuredOK = oc.ActualCost()
				return resp
			})))

	resp := run(t, e, `{ users { id } }`, "")
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
	if !measuredOK {
		t.Fatal("actual cost was not measured")
	}
	if measured != 1+3 {
		t.Fatalf("actual = %d, want 4", measured)
	}
	// Report is off, so nothing reaches the client.
	if _, ok := resp.Extensions["cost"]; ok {
		t.Fatalf("cost leaked into extensions with Report off: %v", resp.Extensions)
	}
}
