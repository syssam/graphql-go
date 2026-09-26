package veloxfx

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/fx"

	"github.com/syssam/graphql-go/examples/veloxfx/velox"
)

// The queries in this file are shaped like what large clients send, not like
// hand-written examples: Relay-compiled named fragments, variables for every
// page size and filter, one connection asked for twice under two aliases,
// and a count-only badge beside a page. Each is run against enough rows that
// a query per row would show, and each asserts both the answer and the SQL
// it took, because the failure this guards is silent -- the answer is right
// and the database does fifty times the work.

// sqlApp is an app whose database logs every statement it runs while
// recording is on.
type sqlApp struct {
	*app
	mu        sync.Mutex
	recording atomic.Bool
	stmts     []string
}

func startRecording(t *testing.T) *sqlApp {
	t.Helper()
	s := &sqlApp{}
	s.app = start(t, fx.Decorate(func(*velox.Client) (*velox.Client, error) {
		c, err := openDB(t, velox.Debug(), velox.Log(func(v ...any) {
			if !s.recording.Load() {
				return
			}
			line := fmt.Sprint(v...)
			if strings.Contains(line, "driver.Query") {
				s.mu.Lock()
				s.stmts = append(s.stmts, line)
				s.mu.Unlock()
			}
		}))
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { c.Close() })
		return c, nil
	}))
	return s
}

// run executes query with vars and returns its data and the statements it
// ran. It fails the test on any GraphQL error.
func (s *sqlApp) run(query string, vars map[string]any) (string, []string) {
	s.t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		s.t.Fatal(err)
	}
	s.mu.Lock()
	s.stmts = nil
	s.mu.Unlock()
	s.recording.Store(true)
	res, err := s.send(body)
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
	if len(out.Errors) > 0 {
		s.t.Fatalf("%s: %+v", query, out.Errors)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(out.Data), append([]string(nil), s.stmts...)
}

// storefront seeds a catalog of three categories with four products each,
// stock for all of them, three customers and orders orders, each with two
// lines -- enough that a query per parent row is plainly visible.
func (s *sqlApp) storefront(orders int) {
	s.t.Helper()
	for c := 1; c <= 3; c++ {
		s.data(fmt.Sprintf(`mutation { createCategory(input: {name: "cat-%d"}) { id } }`, c))
	}
	s.data(`mutation { createWarehouse(input: {name: "North"}) { id } }`)
	for p := 1; p <= 12; p++ {
		cat := (p-1)/4 + 1
		s.data(fmt.Sprintf(`mutation { createProduct(input: {sku: "sku-%02d", name: "product %d", priceCents: %d, categoryID: "%d"}) { id } }`,
			p, p, p*100, cat))
		s.data(fmt.Sprintf(`mutation { createStock(input: {quantity: 1000, warehouseID: "1", productID: "%d"}) { id } }`, p))
	}
	for c := 1; c <= 3; c++ {
		s.data(fmt.Sprintf(`mutation { createCustomer(input: {name: "customer %d", email: "c%d@example.com"}) { id } }`, c, c))
	}
	for o := range orders {
		s.data(fmt.Sprintf(`mutation { placeOrder(input: {customerID: "%d", warehouseID: "1",
			items: [{productID: "%d", quantity: 2}, {productID: "%d", quantity: 1}]}) { id } }`,
			o%3+1, o%12+1, (o+5)%12+1))
	}
}

// stmtsOn returns the statements that read table.
func stmtsOn(stmts []string, table string) []string {
	var out []string
	for _, s := range stmts {
		if strings.Contains(s, "FROM `"+table+"`") || strings.Contains(s, `FROM "`+table+`"`) {
			out = append(out, s)
		}
	}
	return out
}

// An order history page as a Relay client compiles it: every field comes
// from a named fragment, and the page size is a variable. Fifty orders over
// three customers and twelve products must be one query per edge level, no
// COUNT because nothing asked for totalCount, and the same answer the
// spelled-out query gives.
func TestScenarioRelayFragmentsAndVariablesStayFlat(t *testing.T) {
	s := startRecording(t)
	s.storefront(50)

	const fragments = `
		query OrderHistory($first: Int!) {
			orders(first: $first) { edges { node { id ...OrderRow_order } } pageInfo { hasNextPage endCursor } }
		}
		fragment OrderRow_order on Order { status customer { ...CustomerBadge_customer } items { ...LineItem_item } }
		fragment CustomerBadge_customer on Customer { name }
		fragment LineItem_item on OrderItem { quantity product { sku } }`
	const spelled = `{ orders(first: 50) { edges { node { id status customer { name } items { quantity product { sku } } } }
		pageInfo { hasNextPage endCursor } } }`

	viaFragments, stmts := s.run(fragments, map[string]any{"first": 50})
	viaLiteral, _ := s.run(spelled, nil)
	if viaFragments != viaLiteral {
		t.Fatalf("fragments and variables changed the answer:\n%s\n%s", viaFragments, viaLiteral)
	}
	if !strings.Contains(viaFragments, `"customer":{"name":"customer 1"}`) || !strings.Contains(viaFragments, `"sku":"sku-01"`) {
		t.Fatalf("edges answered empty, as an unprojected column would: %.300s", viaFragments)
	}
	// orders, customers, items, products.
	if len(stmts) != 4 {
		t.Errorf("50 orders took %d queries, want 4:\n%s", len(stmts), strings.Join(stmts, "\n"))
	}
	for _, st := range stmts {
		if strings.Contains(st, "COUNT(") {
			t.Errorf("totalCount was not selected, yet the page counted: %s", st)
		}
	}
}

// Projection: a list that shows names reads names. The customer rows carry an
// email the page never selects, and the product rows a price.
func TestScenarioOnlySelectedColumnsAreRead(t *testing.T) {
	s := startRecording(t)
	s.storefront(6)

	_, stmts := s.run(`query($n: Int) { orders(first: $n) { edges { node { customer { name } items { product { sku } } } } } }`,
		map[string]any{"n": 6})
	customers := stmtsOn(stmts, "customers")
	if len(customers) != 1 {
		t.Fatalf("customers were read %d times, want once:\n%s", len(customers), strings.Join(stmts, "\n"))
	}
	if strings.Contains(customers[0], "email") {
		t.Errorf("the customer query read email, which nothing selected: %s", customers[0])
	}
	products := stmtsOn(stmts, "products")
	if len(products) != 1 || strings.Contains(products[0], "price_cents") {
		t.Errorf("the product query should read sku and not price_cents: %v", products)
	}
}

// A dashboard asks for one connection twice: a count badge and a short list.
// The badge needs COUNT and no rows; the list needs rows and no COUNT.
func TestScenarioAliasedBadgeAndList(t *testing.T) {
	s := startRecording(t)
	s.storefront(9)

	got, stmts := s.run(`query Dashboard($recent: Int!) {
		badge: orders { totalCount }
		recent: orders(first: $recent) { edges { node { status customer { name } } } }
	}`, map[string]any{"recent": 3})
	if !strings.Contains(got, `"badge":{"totalCount":9}`) || strings.Count(got, `"status":"PENDING"`) != 3 {
		t.Fatalf("dashboard = %s", got)
	}
	var counts int
	for _, st := range stmts {
		if strings.Contains(st, "COUNT(") {
			counts++
		}
	}
	if counts != 1 {
		t.Errorf("want exactly one COUNT, for the badge; got %d:\n%s", counts, strings.Join(stmts, "\n"))
	}
	// badge: count, plus a keys-only page; recent: orders, customers.
	if len(stmts) > 4 {
		t.Errorf("dashboard took %d queries:\n%s", len(stmts), strings.Join(stmts, "\n"))
	}
}

// A category browser: every category with the first two products of each, a
// nested connection. It is loaded for all categories at once, limited per
// category, not paged once per category.
func TestScenarioNestedConnectionIsOneQueryPerLevel(t *testing.T) {
	s := startRecording(t)
	s.storefront(0)

	got, stmts := s.run(`query Browse($per: Int) { categories { name products(first: $per) { edges { node { sku } } pageInfo { hasNextPage } } } }`,
		map[string]any{"per": 2})
	if strings.Count(got, `"sku"`) != 6 || strings.Count(got, `"hasNextPage":true`) != 3 {
		t.Fatalf("three categories of two products, each with more to come: %s", got)
	}
	if len(stmts) != 2 {
		t.Errorf("categories and their products took %d queries, want 2:\n%s", len(stmts), strings.Join(stmts, "\n"))
	}
}

// A computed field reads a column the client did not select. totalCents is
// quantity times the unit price at order time; a page that shows the total
// and each line's quantity must not have the price projected away.
func TestScenarioComputedFieldSurvivesProjection(t *testing.T) {
	s := startRecording(t)
	s.shop(10, 10)
	s.data(placeTwoBoardsAndKeycaps)

	got, _ := s.run(`{ orders(first: 5) { edges { node { totalCents items { quantity } } } } }`, nil)
	if want := `{"orders":{"edges":[{"node":{"totalCents":11500,"items":[{"quantity":2},{"quantity":1}]}}]}}`; got != want {
		t.Errorf("orders = %s, want %s", got, want)
	}
}

// The same computed field one level down, where no resolver of the example's
// is on the path: customers, each with a page of orders showing totals. The
// declaration on the schema (Loads("items")) is what loads the items -- once
// for every order of every customer.
func TestScenarioComputedFieldInANestedPageIsDeclaredNotCoded(t *testing.T) {
	s := startRecording(t)
	s.storefront(30)

	got, stmts := s.run(`query($n: Int) { customers { name orders(first: $n) { edges { node { totalCents } } } } }`,
		map[string]any{"n": 5})
	if strings.Count(got, `"totalCents"`) != 15 || strings.Contains(got, `"totalCents":0`) {
		t.Fatalf("three customers, five orders each, every total non-zero: %s", got)
	}
	// customers, their orders (five per customer, in one query), the items.
	if len(stmts) != 3 {
		t.Errorf("took %d queries, want 3:\n%s", len(stmts), strings.Join(stmts, "\n"))
	}
	if items := stmtsOn(stmts, "order_items"); len(items) != 1 || !strings.Contains(items[0], "unit_price_cents") {
		t.Errorf("the items must be read once, with their price: %v", items)
	}
}
