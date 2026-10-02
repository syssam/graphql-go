package storefront

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	graphql "github.com/syssam/graphql-go"
)

// run executes query as p and returns the response.
func run(t testing.TB, e *graphql.Executor, p Principal, query string, vars string) *graphql.Response {
	t.Helper()
	req := &graphql.Request{Query: query}
	if vars != "" {
		req.Variables = json.RawMessage(vars)
	}
	return e.Execute(WithPrincipal(context.Background(), p), req)
}

// newExecutor builds the schema with the authorization options and nothing
// else, so a failing test names an authorization bug and not a limit.
func newExecutor(t testing.TB) (*graphql.Executor, *Store) {
	t.Helper()
	s, store, err := New()
	if err != nil {
		t.Fatalf("building schema: %v", err)
	}
	return graphql.NewExecutor(s, ExecutorOptions()...), store
}

// data asserts the exact data payload and that nothing errored.
func data(t testing.TB, resp *graphql.Response, want string) {
	t.Helper()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %v", resp.Errors[0])
	}
	if got := string(resp.Data); got != want {
		t.Fatalf("data =\n%s\nwant\n%s", got, want)
	}
}

// errorContaining asserts that the response carries an error saying substr.
func errorContaining(t testing.TB, resp *graphql.Response, substr string) {
	t.Helper()
	for _, e := range resp.Errors {
		if strings.Contains(e.Message, substr) {
			return
		}
	}
	t.Fatalf("no error mentioning %q; errors = %v, data = %s", substr, resp.Errors, resp.Data)
}

// The schema declares a requirement at every position but Query.health, so
// RequireAuthCoverage has to accept it. Deleting @public from health is the
// one-line way to watch this fail.
func TestSchemaSatisfiesAuthCoverage(t *testing.T) {
	if _, _, err := New(); err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
}

// RequireAuthCoverage's whole value is refusing a schema, so a test that only
// builds the good one proves nothing. This adds a field declaring neither a
// requirement nor @public and requires the build to fail naming it.
func TestAuthCoverageRefusesAnUndeclaredField(t *testing.T) {
	src := graphql.SDL(sdl + "\nextend type Query { unguarded: String! }\n")
	_, err := newSchemaFrom(NewStore(), src,
		// Bound, so the only thing wrong with the field is that it says
		// nothing about who may read it.
		graphql.Query(graphql.Field("unguarded", func(graphql.Root) string { return "" })),
	)
	if err == nil {
		t.Fatal("a field declaring no authorization was accepted")
	}
	if !strings.Contains(err.Error(), "Query.unguarded") {
		t.Fatalf("the error does not name the offending field: %v", err)
	}
}

// Query.health is the only position anyone may reach, and an anonymous caller
// must reach it: a liveness probe carries no token.
func TestHealthIsPublic(t *testing.T) {
	e, _ := newExecutor(t)
	data(t, run(t, e, Anonymous(), `{ health }`, ""), `{"health":"ok"}`)
}

// Everything else refuses an anonymous caller, which is what the
// @authenticated marker buys. The marker composes with @requiresScopes rather
// than replacing it, so this holds for the mutation too.
func TestAnonymousReachesNothingElse(t *testing.T) {
	e, _ := newExecutor(t)
	for _, q := range []string{
		`{ orders { id } }`,
		`{ order(id: "o1") { id } }`,
		`{ customer(id: "c1") { name } }`,
		`mutation { refundOrder(id: "o1", reason: "x") { id } }`,
	} {
		resp := run(t, e, Anonymous(), q, "")
		if len(resp.Errors) == 0 {
			t.Errorf("%s was allowed for an anonymous caller: %s", q, resp.Data)
		}
	}
}

// Staff hold every scope, so the graph answers in full. This is the control:
// every withholding test below differs from it only in the principal.
func TestStaffSeesEverything(t *testing.T) {
	e, _ := newExecutor(t)
	resp := run(t, e, Staff("s1"), `{ order(id: "o2") { reference margin customer { name email } } }`, "")
	data(t, resp, `{"order":{"reference":"SO-0002","margin":"90.00","customer":{"name":"Grace Hopper","email":"grace@example.com"}}}`)
}

// A support agent may read every order and neither margin nor personal data.
// The two are withheld differently on purpose: the address is masked, because
// an agent has to tell two customers apart, and the margin is refused, because
// a wrong number is worse than a refusal.
func TestSupportGetsMaskedEmailAndRefusedMargin(t *testing.T) {
	e, _ := newExecutor(t)

	resp := run(t, e, Support("a1"), `{ order(id: "o2") { customer { name email } } }`, "")
	data(t, resp, `{"order":{"customer":{"name":"Grace Hopper","email":"g****@example.com"}}}`)

	resp = run(t, e, Support("a1"), `{ order(id: "o2") { reference margin } }`, "")
	errorContaining(t, resp, "denied")
}

// Order.margin is non-null, so refusing it bubbles the order away rather than
// writing a null the schema forbids. Pinned because it is the visible cost of
// choosing Deny over a nullable field, and a reader should not have to guess.
func TestRefusingANonNullFieldBubblesItsParent(t *testing.T) {
	e, _ := newExecutor(t)
	resp := run(t, e, Support("a1"), `{ order(id: "o2") { margin } }`, "")
	if got := string(resp.Data); got != `{"order":null}` {
		t.Fatalf("data = %s, want the order nulled by the bubble", got)
	}
}

// A customer sees their own orders: the rows they may not see are dropped
// from the list rather than reported, with no gap and no error. (A page cut
// with `first` is cut before the drop, so it comes back short; see the policy.)
// c1 owns o1 and o3; o2 belongs to c2.
func TestCustomerSeesOnlyTheirOwnOrdersInAList(t *testing.T) {
	e, _ := newExecutor(t)
	resp := run(t, e, CustomerPrincipal("c1"), `{ orders { reference } }`, "")
	data(t, resp, `{"orders":[{"reference":"SO-0003"},{"reference":"SO-0001"}]}`)
}

// The same rule at a single-object position cannot drop -- there is no list
// to drop out of -- so it answers null, with no error: exactly what an id
// that does not exist answers. Refusing instead would be an error the missing
// id does not get, and that difference tells a customer which order ids are
// real however the refusal is worded.
func TestSomeoneElsesOrderAnswersLikeOneThatDoesNotExist(t *testing.T) {
	e, _ := newExecutor(t)
	for _, tc := range []struct{ field, someoneElses string }{
		{`order(id: %q) { reference }`, "o2"},
		{`customer(id: %q) { name }`, "c2"},
	} {
		field := tc.field
		foreign := run(t, e, CustomerPrincipal("c1"), "{ "+fmt.Sprintf(field, tc.someoneElses)+" }", "")
		missing := run(t, e, CustomerPrincipal("c1"), "{ "+fmt.Sprintf(field, "no-such-id")+" }", "")
		if len(foreign.Errors) != 0 || len(missing.Errors) != 0 {
			t.Fatalf("%s: errors foreign=%v missing=%v, want none for either", field, foreign.Errors, missing.Errors)
		}
		if string(foreign.Data) != string(missing.Data) {
			t.Fatalf("%s: someone else's row answers %s and a missing one %s", field, foreign.Data, missing.Data)
		}
	}
}

// Dropping must renumber: an error reported later has to carry the index the
// client actually sees, not the one the row had before its predecessors were
// dropped. c1's listing drops o2, so SO-0001 is index 1 to the client.
func TestDroppedRowsRenumberTheOnesAfterThem(t *testing.T) {
	e, _ := newExecutor(t)
	resp := run(t, e, CustomerPrincipal("c1"), `{ orders { reference margin } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("margin was allowed for a customer")
	}
	for _, err := range resp.Errors {
		if len(err.Path) < 2 || !err.Path[1].IsIndex {
			t.Fatalf("error path %v does not index the list", err.Path)
		}
		if err.Path[1].Index > 1 {
			t.Fatalf("error path %v indexes past the two rows the client can see", err.Path)
		}
	}
}

// The inference rule, and the reason an argument is a site of its own: a
// caller who may not select Order.margin may not filter by it either, or they
// can recover the number a comparison at a time.
func TestFilteringByMarginNeedsTheSameScopeAsReadingIt(t *testing.T) {
	e, _ := newExecutor(t)
	const q = `query($w: OrderWhere) { orders(where: $w) { reference } }`

	resp := run(t, e, Support("a1"), q, `{"w":{"minMargin":"50.00"}}`)
	errorContaining(t, resp, "denied")

	// The control: the same caller, the same argument, a key they may use.
	resp = run(t, e, Support("a1"), q, `{"w":{"status":"PAID"}}`)
	data(t, resp, `{"orders":[{"reference":"SO-0002"}]}`)

	// And the scope that lifts it.
	resp = run(t, e, Staff("s1"), q, `{"w":{"minMargin":"50.00"}}`)
	data(t, resp, `{"orders":[{"reference":"SO-0002"}]}`)
}

// The mutation carries @requiresScopes on top of the type-level marker, so
// both have to be satisfied.
func TestRefundNeedsOrderWrite(t *testing.T) {
	e, store := newExecutor(t)

	resp := run(t, e, Support("a1"), `mutation { refundOrder(id: "o1", reason: "damaged") { status } }`, "")
	errorContaining(t, resp, "denied")
	if store.Order("o1").Status == StatusRefunded {
		t.Fatal("the refund ran despite being denied")
	}

	resp = run(t, e, Staff("s1"), `mutation { refundOrder(id: "o1", reason: "damaged") { status } }`, "")
	data(t, resp, `{"refundOrder":{"status":"REFUNDED"}}`)
}

// The loader has to keep batching underneath the row checks: the instance
// policy drains a list and decides it in one call before any element is
// written, and the customer fetches happen after that. Three orders over two
// customers must be one fetch of two keys, not three fetches -- the number to
// alert on in a real deployment, where one key per fetch means batching has
// degraded to N+1.
func TestCustomerLoaderBatchesUnderTheRowChecks(t *testing.T) {
	e, store := newExecutor(t)

	resp := run(t, e, Staff("s1"), `{ orders { reference customer { name } } }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %v", resp.Errors[0])
	}
	fetches, keys := store.CustomerFetches()
	if fetches != 1 {
		t.Errorf("the customer store was read %d times, want 1 batch for 3 orders", fetches)
	}
	if keys != 2 {
		t.Errorf("the batch carried %d keys, want the 2 distinct customers", keys)
	}
}

// The WRITE site. Ordering on someone else's behalf is a staff action, and
// the rule is written as "was this key supplied" rather than "whose id is
// it", because Decision.Input reports key paths, enum values and explicit
// nulls and never scalar values. A schema whose policy needs the value has
// been shaped wrong; this one makes the key itself the signal.
func TestPlacingForSomeoneElseNeedsOrderWrite(t *testing.T) {
	const q = `mutation($in: PlaceOrderInput!) { placeOrder(input: $in) { reference customer { id } } }`
	lines := `"lines":[{"sku":"KB-01","description":"Keyboard","quantity":1,"unitPrice":"10.00"}]`

	// A customer ordering for themselves does not name themselves.
	e, _ := newExecutor(t)
	resp := run(t, e, CustomerPrincipal("c1"), q, `{"in":{`+lines+`}}`)
	data(t, resp, `{"placeOrder":{"reference":"SO-0004","customer":{"id":"c1"}}}`)

	// The same customer naming someone else is refused, and the mutation does
	// not run: an argument site decides whether the field may run at all.
	e, store := newExecutor(t)
	before := len(store.Orders(nil, 0))
	resp = run(t, e, CustomerPrincipal("c1"), q, `{"in":{"customerId":"c2",`+lines+`}}`)
	errorContaining(t, resp, "denied")
	if after := len(store.Orders(nil, 0)); after != before {
		t.Fatalf("the store grew from %d to %d orders despite the refusal", before, after)
	}

	// Staff hold order:write, so naming a customer is allowed.
	e, _ = newExecutor(t)
	resp = run(t, e, Staff("s1"), q, `{"in":{"customerId":"c2",`+lines+`}}`)
	data(t, resp, `{"placeOrder":{"reference":"SO-0004","customer":{"id":"c2"}}}`)
}
