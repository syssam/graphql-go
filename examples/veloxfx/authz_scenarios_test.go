package veloxfx

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Authorization in a real API is three different things, and each has a
// place it belongs. These pin all three, and what each costs in SQL: a
// position someone may not read must cost no query, and a row someone may
// not see must be filtered in the database, not loaded and dropped.

// runAs is run as a given viewer, returning the GraphQL errors instead of
// failing on them.
func (s *sqlApp) runAs(viewer, query string, vars map[string]any) (string, []string, []string) {
	s.t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		s.t.Fatal(err)
	}
	s.mu.Lock()
	s.stmts = nil
	s.mu.Unlock()
	s.recording.Store(true)
	res, err := s.as(viewer).send(body)
	if err != nil {
		s.t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	s.recording.Store(false)
	if err != nil {
		s.t.Fatal(err)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		s.t.Fatalf("%s: %v", raw, err)
	}
	var codes []string
	for _, e := range out.Errors {
		codes = append(codes, e.Extensions.Code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(out.Data), append([]string(nil), s.stmts...), codes
}

// A shopper sees products but not stock levels: Product.stocks needs
// inventory:read, the policy answers an empty list, and -- because the
// decision is made before anything resolves -- velox never queries stocks.
// Staff see them, in one query for the whole page.
func TestScenarioAWithheldEdgeIsNotQueried(t *testing.T) {
	s := startRecording(t)
	s.storefront(0)
	const q = `{ products(first: 12) { edges { node { sku stocks { quantity } } } } }`

	got, stmts, errs := s.runAs("", q, nil)
	if len(errs) > 0 || strings.Count(got, `"stocks":[]`) != 12 {
		t.Fatalf("a shopper should see twelve products with empty stocks: %s %v", got, errs)
	}
	if n := len(stmtsOn(stmts, "stocks")); n != 0 {
		t.Errorf("stocks were queried %d times for a viewer who may not see them:\n%s", n, strings.Join(stmts, "\n"))
	}

	got, stmts, _ = s.runAs("staff", q, nil)
	if strings.Count(got, `"quantity":1000`) != 12 {
		t.Fatalf("staff should see every stock level: %s", got)
	}
	if n := len(stmtsOn(stmts, "stocks")); n != 1 {
		t.Errorf("staff: stocks queried %d times, want once for the page", n)
	}
}

// Personal data is masked, not removed: a support view still tells two
// customers apart. The column is still read -- Redact rewrites a resolved
// value -- so this is about what leaves the server, not what it loads.
func TestScenarioPersonalDataIsMasked(t *testing.T) {
	s := startRecording(t)
	s.storefront(0)
	const q = `{ customers { name email } }`

	got, _, _ := s.runAs("customer:1", q, nil)
	if !strings.Contains(got, `"email":"c*@example.com"`) || strings.Contains(got, `c1@example.com`) {
		t.Errorf("without customer:pii the email is masked: %s", got)
	}
	got, _, _ = s.runAs("staff", q, nil)
	if !strings.Contains(got, `"email":"c1@example.com"`) {
		t.Errorf("staff see the address: %s", got)
	}
}

// A customer sees only their own orders -- through the list, its count, a
// lookup by id, and another customer's edge -- and the filter is in the SQL
// of every one, so their page costs the same whatever else the table holds.
func TestScenarioCustomersSeeOnlyTheirOwnOrders(t *testing.T) {
	s := startRecording(t)
	s.storefront(30) // ten orders for each of three customers

	got, stmts, errs := s.runAs("customer:2", `{ orders(first: 50) { totalCount edges { node { id customer { id } } } } }`, nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	var page struct {
		Orders struct {
			TotalCount int
			Edges      []struct {
				Node struct {
					ID       string
					Customer struct{ ID string }
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(got), &page); err != nil {
		t.Fatal(err)
	}
	if page.Orders.TotalCount != 10 || len(page.Orders.Edges) != 10 {
		t.Fatalf("customer 2 has ten orders; saw %d of a count of %d", len(page.Orders.Edges), page.Orders.TotalCount)
	}
	var theirs string
	for _, e := range page.Orders.Edges {
		if e.Node.Customer.ID != "2" {
			t.Errorf("order %s of customer %s reached customer 2", e.Node.ID, e.Node.Customer.ID)
		}
	}
	for _, st := range stmtsOn(stmts, "orders") {
		if !strings.Contains(st, "customer_orders") || !strings.Contains(st, "WHERE") {
			t.Errorf("an order read without the ownership filter in SQL: %s", st)
		}
	}

	// Someone else's order by id is simply not there.
	all, _, _ := s.runAs("staff", `{ orders(first: 50) { edges { node { id customer { id } } } } }`, nil)
	for _, part := range strings.Split(all, `{"node":`)[1:] {
		if strings.Contains(part, `"customer":{"id":"1"}`) {
			theirs = strings.Split(strings.Split(part, `"id":"`)[1], `"`)[0]
			break
		}
	}
	if got, _, _ := s.runAs("customer:2", fmt.Sprintf(`{ order(id: %q) { id } }`, theirs), nil); got != `{"order":null}` {
		t.Errorf("customer 2 read customer 1's order %s: %s", theirs, got)
	}
	// And through another customer's edge.
	if got, _, _ := s.runAs("customer:2", `{ customer(id: "1") { orders { totalCount } } }`, nil); got != `{"customer":{"orders":{"totalCount":0}}}` {
		t.Errorf("customer 2 counted customer 1's orders: %s", got)
	}
}

// Order reads fail closed: nobody signed in reads none, and the database is
// not asked.
func TestScenarioAnonymousOrderReadsFailClosed(t *testing.T) {
	s := startRecording(t)
	s.storefront(3)
	_, stmts, errs := s.runAs("", `{ orders { totalCount } }`, nil)
	if len(errs) != 1 || errs[0] != "UNAUTHENTICATED" {
		t.Fatalf("an anonymous order read must be one UNAUTHENTICATED error, got %v", errs)
	}
	if n := len(stmtsOn(stmts, "orders")); n != 0 {
		t.Errorf("an anonymous order read reached the database %d times", n)
	}
}

// A query built to be expensive is refused from its plan, before any SQL:
// too deep, or asking for more rows than the budget allows. Refused, it
// costs a walk of the document; answered, it would have been millions of
// rows.
func TestScenarioAbusiveQueriesCostNoSQL(t *testing.T) {
	s := startRecording(t)
	s.storefront(3)
	for name, tc := range map[string]struct{ query, code string }{
		// Cheap -- one row a level -- but fourteen levels deep: only the
		// depth limit refuses it.
		"deep": {`{ product(id: "1") { category { products(first: 1) { edges { node { category {
			products(first: 1) { edges { node { category { products(first: 1) { edges { node { sku } } } } } } } } } } } } } }`,
			"MAX_DEPTH_EXCEEDED"},
		// Shallow, but a thousand orders of every customer, each with its
		// items, products and stock: only the cost limit refuses it.
		"wide": {`{ customers { orders(first: 1000) { edges { node { items { product { stocks { quantity } } } } } } } }`,
			"COMPLEXITY_LIMIT_EXCEEDED"},
	} {
		t.Run(name, func(t *testing.T) {
			_, stmts, errs := s.runAs("staff", tc.query, nil)
			if len(errs) != 1 || errs[0] != tc.code {
				t.Fatalf("want one %s error, got %v", tc.code, errs)
			}
			if len(stmts) != 0 {
				t.Errorf("a refused query ran %d statements", len(stmts))
			}
		})
	}
	// A page a real client asks for stays well inside the budget.
	if _, _, errs := s.runAs("staff", `{ categories { products(first: 20) { edges { node { sku stocks { quantity } } } } } }`, nil); len(errs) > 0 {
		t.Errorf("an ordinary browse page was refused: %v", errs)
	}
}

// The introspection query GraphiQL, Apollo tooling and client generators
// send nests its type references seven levels deep and, priced by list
// sizes, costs more than the budget above allows any real page. It is
// answered anyway -- introspection is bounded by its own rule, not by limits
// set against the API's queries -- and costs no SQL. A production deployment
// that must not describe itself sets graphql.DisableIntrospection().
func TestScenarioToolsCanIntrospectUnderProductionLimits(t *testing.T) {
	s := startRecording(t)
	got, stmts, errs := s.runAs("", toolIntrospection, nil)
	if len(errs) > 0 {
		t.Fatalf("the standard introspection query was refused: %v", errs)
	}
	if !strings.Contains(got, `"name":"Order"`) || len(stmts) != 0 {
		t.Errorf("introspection: %d statements, %.200s", len(stmts), got)
	}
	// Recursing through type members is what makes introspection expensive,
	// and that is refused.
	if _, _, errs := s.runAs("", `{ __schema { types { fields { type { fields { type { fields { name } } } } } } } }`, nil); len(errs) != 1 {
		t.Errorf("a recursive introspection query: %v", errs)
	}
}

const toolIntrospection = `query IntrospectionQuery {
  __schema {
    queryType { name } mutationType { name } subscriptionType { name }
    types { ...FullType }
    directives { name description locations args { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name description
  fields(includeDeprecated: true) { name description args { ...InputValue } type { ...TypeRef } isDeprecated deprecationReason }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name description type { ...TypeRef } defaultValue }
fragment TypeRef on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } } }`
