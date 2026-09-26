package veloxfx

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// This service is a Federation v2 subgraph. A router resolves references to
// its products, customers and orders held by other subgraphs -- reviews that
// name a product, invoices that name an order -- by batching them into one
// _entities call. These pin what that call costs and what it may return.

func reps(typename string, ids ...any) []map[string]any {
	out := make([]map[string]any, len(ids))
	for i, id := range ids {
		out[i] = map[string]any{"__typename": typename, "id": id}
	}
	return out
}

const entitiesQuery = `query($reps: [_Any!]!) { _entities(representations: $reps) {
	__typename
	... on Product { sku category { name } }
	... on Customer { name }
	... on Order { id status }
} }`

// A reviews subgraph's page names twelve products, one of them twice and one
// that was deleted since. The router sends all of them in one call: one query
// for the products, one for their categories, the columns the router asked
// for and no others, and the answers in the order asked with null for the
// deleted one.
func TestScenarioRouterBatchIsOneQueryPerType(t *testing.T) {
	s := startRecording(t)
	s.storefront(0)
	ids := []any{"3", "1", "999", "12", "3"}
	for i := 4; i <= 11; i++ {
		ids = append(ids, fmt.Sprint(i))
	}
	got, stmts, errs := s.runAs("staff", entitiesQuery, map[string]any{"reps": reps("Product", ids...)})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	var out struct {
		Entities []*struct {
			Sku      string
			Category struct{ Name string }
		} `json:"_entities"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entities) != len(ids) {
		t.Fatalf("%d answers for %d representations: %s", len(out.Entities), len(ids), got)
	}
	if out.Entities[0].Sku != "sku-03" || out.Entities[1].Sku != "sku-01" || out.Entities[2] != nil || out.Entities[4].Sku != "sku-03" {
		t.Errorf("answers out of request order, or the deleted product not null: %s", got)
	}
	if out.Entities[3].Category.Name != "cat-3" {
		t.Errorf("product 12's category: %s", got)
	}
	products := stmtsOn(stmts, "products")
	if len(products) != 1 || len(stmtsOn(stmts, "categories")) != 1 || len(stmts) != 2 {
		t.Fatalf("thirteen representations took %d queries, want 2 (products, categories):\n%s", len(stmts), strings.Join(stmts, "\n"))
	}
	if strings.Contains(products[0], "price_cents") {
		t.Errorf("the router did not ask for prices, but they were read: %s", products[0])
	}
}

// One call can carry several types; each is one query.
func TestScenarioRouterBatchOfSeveralTypes(t *testing.T) {
	s := startRecording(t)
	s.storefront(0)
	all := append(reps("Product", "1", "2"), reps("Customer", "1", "3", "2")...)
	got, stmts, errs := s.runAs("staff", entitiesQuery, map[string]any{"reps": all})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(got, `{"__typename":"Customer","name":"customer 3"}`) {
		t.Errorf("customers: %s", got)
	}
	if n, c := len(stmtsOn(stmts, "products")), len(stmtsOn(stmts, "customers")); n != 1 || c != 1 {
		t.Errorf("products queried %d times and customers %d, want once each:\n%s", n, c, strings.Join(stmts, "\n"))
	}
}

// Reaching an order through the router is reaching it: the ownership filter
// applies to _entities as to every other read, in the SQL. A customer's
// token forwarded by the router yields their own orders and nulls for the
// rest; nobody's yields nothing.
func TestScenarioRouterFetchKeepsOwnership(t *testing.T) {
	s := startRecording(t)
	s.storefront(9) // three orders each for customers 1, 2 and 3
	var ids []any
	for i := 1; i <= 9; i++ {
		ids = append(ids, fmt.Sprint(i))
	}
	got, stmts, errs := s.runAs("customer:2", entitiesQuery, map[string]any{"reps": reps("Order", ids...)})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	var out struct {
		Entities []*struct{ ID string } `json:"_entities"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range out.Entities {
		if e != nil {
			seen = append(seen, e.ID)
		}
	}
	// storefront's loop places order o+1 for customer o%3+1, so customer 2
	// owns orders 2, 5 and 8.
	if strings.Join(seen, ",") != "2,5,8" {
		t.Errorf("customer 2 reached orders %v through the router, want 2,5,8: %s", seen, got)
	}
	orders := stmtsOn(stmts, "orders")
	if len(orders) != 1 || !strings.Contains(orders[0], "customer_orders") {
		t.Errorf("the router's order fetch must be one query carrying the ownership filter: %v", orders)
	}

	if _, _, errs := s.runAs("", entitiesQuery, map[string]any{"reps": reps("Order", "1")}); len(errs) != 1 || errs[0] != "UNAUTHENTICATED" {
		t.Errorf("an anonymous fetch of an order through the router: %v", errs)
	}
}

// The router composes from _service: it must carry exactly one Federation v2
// link -- velox writes it once a type has @key; a second at another version
// fails composition -- and the key of every entity.
func TestScenarioServiceSDLIsComposable(t *testing.T) {
	s := startRecording(t)
	got, _, errs := s.runAs("", `{ _service { sdl } }`, nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if n := strings.Count(got, "specs.apollo.dev/federation/v2"); n != 1 {
		t.Errorf("_service carries %d federation links, want 1", n)
	}
	for _, typ := range []string{"Product", "Customer", "Order"} {
		// The type's own header, from its name to its opening brace.
		i := strings.Index(got, "type "+typ+" ")
		if i < 0 {
			t.Errorf("_service lacks type %s", typ)
			continue
		}
		header, _, _ := strings.Cut(got[i:], "{")
		if !strings.Contains(header, `@key(fields: \"id\")`) {
			t.Errorf("_service lacks %s's key: %s", typ, header)
		}
	}
}
