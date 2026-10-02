package veloxfx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/syssam/graphql-go/examples/veloxfx/graph"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/service"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
)

// The generated bindings must match the SDL velox wrote, and a clean build
// says nothing about that; ValidateSchema does, with no database behind it.
func TestSchemaBinds(t *testing.T) {
	if err := graph.ValidateSchema(scalars); err != nil {
		t.Fatal(err)
	}
}

// Every entity is one package, graph/<entity>: gqlc's generated.go beside the
// Handler written in <entity>.resolvers.go. No root field stayed in root: the
// ones velox declares follow the type they return, and the hand-written ones
// in sdl/ follow their file.
func TestEveryEntityIsItsOwnGroup(t *testing.T) {
	dirs, err := os.ReadDir("graph")
	if err != nil {
		t.Fatal(err)
	}
	var groups []string
	for _, d := range dirs {
		if d.IsDir() && d.Name() != "model" && d.Name() != "schema" {
			groups = append(groups, d.Name())
		}
	}
	want := []string{"category", "customer", "order", "orderitem", "product", "root", "stock", "warehouse"}
	if !slices.Equal(groups, want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
	for _, g := range want {
		if g == "root" {
			continue // velox's shared types: nothing to resolve
		}
		if _, err := os.Stat(filepath.Join("graph", g, g+".resolvers.go")); err != nil {
			t.Errorf("group %s has no Handler beside its generated code: %v", g, err)
		}
	}
	read := func(g string) string {
		b, err := os.ReadFile(filepath.Join("graph", g, "generated.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if root := read("root"); strings.Contains(root, "type Resolver interface") {
		t.Errorf("root still holds resolver fields:\n%s", root)
	}
	for g, method := range map[string]string{
		"product": "DeleteProduct(ctx",
		"order":   "CancelOrder(ctx",
		"stock":   "AdjustStock(ctx",
	} {
		if !strings.Contains(read(g), method) {
			t.Errorf("%s is not in the %s group", method, g)
		}
	}
}

type app struct {
	t   *testing.T
	url string
	// viewer is the X-Viewer header each request sends: "staff" unless a
	// test asks as someone else with as.
	viewer string
}

// as returns the app acting as viewer ("" is anonymous).
func (a *app) as(viewer string) *app {
	c := *a
	c.viewer = viewer
	return &c
}

// start runs the whole Module on a free port against a database no other
// test shares, and stops it when the test ends.
func start(t *testing.T, opts ...fx.Option) *app {
	t.Helper()
	var srv *Server
	a := fxtest.New(t, append([]fx.Option{
		Module,
		fx.Supply(testConfig(t)),
		fx.Populate(&srv),
		fx.NopLogger,
	}, opts...)...)
	a.RequireStart()
	t.Cleanup(a.RequireStop)
	// Runs first: connections the client opened ahead of need would make
	// every stop wait out http.Server's grace for them. The server handles
	// that (TestStopWaitsOutAnUnusedConnection); tests need not pay for it.
	t.Cleanup(http.DefaultClient.CloseIdleConnections)
	return &app{t: t, url: "http://" + srv.Addr() + "/graphql", viewer: "staff"}
}

type response struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

func (a *app) post(query string) response {
	a.t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		a.t.Fatal(err)
	}
	res, err := a.send(body)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("%d %s: %v", res.StatusCode, raw, err)
	}
	return out
}

// send posts a GraphQL request body as the app's viewer.
func (a *app) send(body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.viewer != "" {
		req.Header.Set(viewer.Header, a.viewer)
	}
	return http.DefaultClient.Do(req)
}

func (a *app) data(query string) string {
	a.t.Helper()
	r := a.post(query)
	if len(r.Errors) > 0 {
		a.t.Fatalf("%s: %+v", query, r.Errors)
	}
	return string(r.Data)
}

// refused runs a query that must fail with one error whose extensions.code is
// code and whose message contains want. The code is what a client branches
// on; the message is for a person.
func (a *app) refused(query, code, want string) {
	a.t.Helper()
	r := a.post(query)
	if len(r.Errors) != 1 || r.Errors[0].Extensions.Code != code || !strings.Contains(r.Errors[0].Message, want) {
		a.t.Errorf("%s: want one %s error containing %q, got %+v (data %s)", query, code, want, r.Errors, r.Data)
	}
}

// shop builds the catalog and the stock, and no orders: two products in one
// category, stocked in one warehouse, and one customer.
func (a *app) shop(boards, keycaps int) {
	a.t.Helper()
	a.data(`mutation { createCategory(input: {name: "Keyboards"}) { id } }`)
	a.data(`mutation { createProduct(input: {sku: "kb-1", name: "Board", priceCents: 5000, categoryID: "1"}) { id } }`)
	a.data(`mutation { createProduct(input: {sku: "kb-2", name: "Keycaps", priceCents: 1500, categoryID: "1"}) { id } }`)
	a.data(`mutation { createWarehouse(input: {name: "North"}) { id } }`)
	a.data(fmt.Sprintf(`mutation { createStock(input: {quantity: %d, warehouseID: "1", productID: "1"}) { id } }`, boards))
	a.data(fmt.Sprintf(`mutation { createStock(input: {quantity: %d, warehouseID: "1", productID: "2"}) { id } }`, keycaps))
	a.data(`mutation { createCustomer(input: {name: "Ada", email: "ada@example.com"}) { id } }`)
}

const placeTwoBoardsAndKeycaps = `mutation { placeOrder(input: {customerID: "1", warehouseID: "1",
	items: [{productID: "1", quantity: 2}, {productID: "2", quantity: 1}]}) { id } }`

// seed is shop plus three orders of two lines each, placed the only way an
// order can be: placeOrder. Stock ends at 7 boards and no keycaps.
func (a *app) seed() {
	a.t.Helper()
	a.shop(13, 3)
	for range 3 {
		a.data(placeTwoBoardsAndKeycaps)
	}
}

// velox would generate a create and an update for every entity, writing any
// column and attaching any row by id. An order's status, an item's price and
// a stock count carry rules, so those writes are operations instead, and no
// input can move a row from one parent to another.
func TestTheAPIOffersOperationsNotRawWrites(t *testing.T) {
	a := start(t)
	var mutation struct {
		Type struct{ Fields []struct{ Name string } } `json:"__type"`
	}
	if err := json.Unmarshal([]byte(a.data(`{ __type(name: "Mutation") { fields { name } } }`)), &mutation); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range mutation.Type.Fields {
		have[f.Name] = true
	}
	for _, name := range []string{"createOrder", "updateOrder", "createOrderItem", "updateOrderItem", "updateStock"} {
		if have[name] {
			t.Errorf("Mutation.%s bypasses a business rule and must not exist", name)
		}
	}
	for _, name := range []string{
		"placeOrder", "payOrder", "shipOrder", "cancelOrder", "deleteOrder", "adjustStock",
		"updateCategory", "updateCustomer", "updateProduct", "updateWarehouse",
	} {
		if !have[name] {
			t.Errorf("Mutation.%s is missing", name)
		}
	}

	for _, input := range []string{"CreateCategoryInput", "UpdateCategoryInput", "CreateProductInput", "UpdateProductInput",
		"CreateCustomerInput", "UpdateCustomerInput", "CreateWarehouseInput", "UpdateWarehouseInput"} {
		got := a.data(fmt.Sprintf(`{ __type(name: %q) { inputFields { name } } }`, input))
		// A list of ids, or adding and removing by id, attaches existing rows. A
		// nullable edge's clearX sets the row's own foreign key and is allowed.
		for _, bad := range []string{`IDs"`, `"add`, `"remove`} {
			if strings.Contains(got, bad) {
				t.Errorf("%s lets a client move rows between parents: %s", input, got)
			}
		}
	}
}

func TestServesEveryEntity(t *testing.T) {
	a := start(t)
	a.seed()

	// A created row's edges are left unloaded, so they answer with the real
	// row: an id-only stub marked loaded would say "name":"".
	got := a.data(`mutation { createProduct(input: {sku: "kb-3", name: "Switches", priceCents: 900, categoryID: "1"}) { sku category { name } } }`)
	if want := `{"createProduct":{"sku":"kb-3","category":{"name":"Keyboards"}}}`; got != want {
		t.Errorf("createProduct = %s, want %s", got, want)
	}

	// sales -> catalog, and the computed totalCents from sdl/order.graphql.
	got = a.data(`{ orders { edges { node { status totalCents customer { name } items { quantity unitPriceCents product { sku category { name } } } } } } }`)
	order := `{"status":"PENDING","totalCents":11500,"customer":{"name":"Ada"},"items":[` +
		`{"quantity":2,"unitPriceCents":5000,"product":{"sku":"kb-1","category":{"name":"Keyboards"}}},` +
		`{"quantity":1,"unitPriceCents":1500,"product":{"sku":"kb-2","category":{"name":"Keyboards"}}}]}`
	node := `{"node":` + order + `}`
	if want := `{"orders":{"edges":[` + node + `,` + node + `,` + node + `]}}`; got != want {
		t.Errorf("orders = %s", got)
	}

	// inventory -> catalog, and -> sales through the orders taken from it.
	got = a.data(`{ warehouse(id: "1") { name stocks { quantity product { name } } orders { totalCount } } }`)
	if want := `{"warehouse":{"name":"North","stocks":[{"quantity":7,"product":{"name":"Board"}},{"quantity":0,"product":{"name":"Keycaps"}}],"orders":{"totalCount":3}}}`; got != want {
		t.Errorf("warehouse = %s", got)
	}

	for _, q := range []string{
		`mutation { updateCategory(id: "1", input: {name: "Boards"}) { name } }`,
		`mutation { updateCustomer(id: "1", input: {email: "ada@example.org"}) { email } }`,
		`mutation { updateWarehouse(id: "1", input: {name: "North 2"}) { name } }`,
		`mutation { updateProduct(id: "3", input: {priceCents: 950}) { priceCents } }`,
	} {
		a.data(q)
	}
	if got := a.data(`{ category(id: "1") { name } customer(id: "1") { email } }`); got != `{"category":{"name":"Boards"},"customer":{"email":"ada@example.org"}}` {
		t.Errorf("after updates = %s", got)
	}
}

// Every list root loads what the query selects beneath it, across entities,
// so one request is a fixed number of queries however many rows it returns.
// Three orders of two items each, loaded one edge at a time, is 1 + 3
// customers + 3 item lists + 6 products = 13 queries; collected it is 4, and
// totalCents reuses the items already loaded. No COUNT(*): totalCount is not
// selected, and velox reads that from this engine through graphqlgo.Collect.
func TestOrdersAreAFixedNumberOfQueries(t *testing.T) {
	var queries, counting atomic.Int64
	a := start(t, fx.Decorate(func(*velox.Client) (*velox.Client, error) {
		counted, err := openDB(t, velox.Debug(), velox.Log(func(v ...any) {
			if counting.Load() == 1 && strings.Contains(fmt.Sprint(v...), "driver.Query") {
				queries.Add(1)
			}
		}))
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { counted.Close() })
		return counted, nil
	}))
	a.seed()

	counting.Store(1)
	a.data(`{ orders { edges { node { totalCents customer { name } items { product { name } } } } } }`)
	counting.Store(0)
	if n := queries.Load(); n != 4 {
		t.Errorf("orders ran %d queries, want 4 (orders, customers, items, products)", n)
	}
}

// products is velox's Relay connection: a page, a cursor to the next one,
// a filter over the columns schema/catalog.go opted in, and an order.
func TestProductsPageFilterAndOrder(t *testing.T) {
	a := start(t)
	a.data(`mutation { createCategory(input: {name: "Keyboards"}) { id } }`)
	a.data(`mutation { createCategory(input: {name: "Mice"}) { id } }`)
	for i, p := range []struct {
		sku   string
		price int
		cat   int
	}{{"kb-1", 5000, 1}, {"kb-2", 1500, 1}, {"kb-3", 900, 1}, {"ms-1", 2500, 2}, {"ms-2", 4000, 2}} {
		a.data(fmt.Sprintf(`mutation { createProduct(input: {sku: %q, name: "p%d", priceCents: %d, categoryID: "%d"}) { id } }`, p.sku, i, p.price, p.cat))
	}

	// Most expensive first, two at a time.
	first := a.data(`{ products(first: 2, orderBy: {field: PRICE, direction: DESC}) {
		totalCount edges { node { sku priceCents } } pageInfo { hasNextPage endCursor } } }`)
	var page struct {
		Products struct {
			TotalCount int
			Edges      []struct{ Node struct{ Sku string } }
			PageInfo   struct {
				HasNextPage bool
				EndCursor   string
			}
		}
	}
	if err := json.Unmarshal([]byte(first), &page); err != nil {
		t.Fatal(err)
	}
	if got := skus(page.Products.Edges); got != "kb-1,ms-2" || page.Products.TotalCount != 5 || !page.Products.PageInfo.HasNextPage {
		t.Fatalf("first page = %s", first)
	}

	// The cursor continues from where the page ended, in the same order.
	next := a.data(fmt.Sprintf(`{ products(first: 2, after: %q, orderBy: {field: PRICE, direction: DESC}) { edges { node { sku } } } }`,
		page.Products.PageInfo.EndCursor))
	if want := `{"products":{"edges":[{"node":{"sku":"ms-1"}},{"node":{"sku":"kb-2"}}]}}`; next != want {
		t.Errorf("second page = %s, want %s", next, want)
	}

	// where combines a column predicate with an edge predicate.
	cheap := a.data(`{ products(where: {priceCentsLT: 2000, hasCategoryWith: [{name: "Keyboards"}]}, orderBy: {field: PRICE}) { edges { node { sku } } } }`)
	if want := `{"products":{"edges":[{"node":{"sku":"kb-3"}},{"node":{"sku":"kb-2"}}]}}`; cheap != want {
		t.Errorf("cheap keyboards = %s, want %s", cheap, want)
	}

	// An edge can be a connection too: Category.products pages within the
	// category.
	nested := a.data(`{ category(id: "2") { name products(orderBy: {field: NAME, direction: DESC}) { totalCount edges { node { sku } } } } }`)
	if want := `{"category":{"name":"Mice","products":{"totalCount":2,"edges":[{"node":{"sku":"ms-2"}},{"node":{"sku":"ms-1"}}]}}}`; nested != want {
		t.Errorf("category products = %s, want %s", nested, want)
	}
}

func skus(edges []struct{ Node struct{ Sku string } }) string {
	var s []string
	for _, e := range edges {
		s = append(s, e.Node.Sku)
	}
	return strings.Join(s, ",")
}

// Every entity reads by id, and what a delete may take with it is a decision
// per entity, not a default.
func TestReadAndDeleteByID(t *testing.T) {
	a := start(t)
	a.seed()

	// An id naming nothing is a null, not an error; one that is not an id
	// at all is an error.
	if got := a.data(`{ product(id: "1") { sku } missing: product(id: "999") { sku } }`); got != `{"product":{"sku":"kb-1"},"missing":null}` {
		t.Errorf("product by id = %s", got)
	}
	a.refused(`{ product(id: "kb-1") { sku } }`, "GRAPHQL_VALIDATION_FAILED", "ID cannot represent value")

	// Refused while referenced, with the reason rather than the driver's text.
	a.refused(`mutation { deleteCategory(id: "1") }`, "FAILED_PRECONDITION", "still referenced")  // products are in it
	a.refused(`mutation { deleteCustomer(id: "1") }`, "FAILED_PRECONDITION", "still referenced")  // she has orders
	a.refused(`mutation { deleteProduct(id: "1") }`, "FAILED_PRECONDITION", "still referenced")   // stocked and ordered
	a.refused(`mutation { deleteWarehouse(id: "1") }`, "FAILED_PRECONDITION", "still referenced") // stock, and orders taken from it

	// An order is a record of a sale: it deletes only once cancelled, and
	// then takes its items with it.
	a.refused(`mutation { deleteOrder(id: "1") }`, "FAILED_PRECONDITION", "order 1 is PENDING")
	a.data(`mutation { cancelOrder(id: "1") { status } }`)
	if got := a.data(`mutation { deleteOrder(id: "1") }`); got != `{"deleteOrder":"1"}` {
		t.Errorf("deleteOrder = %s", got)
	}
	if got := a.data(`{ order(id: "1") { id } orderItems { order { id } } }`); strings.Contains(got, `"id":"1"`) {
		t.Errorf("order 1 or its items survived: %s", got)
	}

	// Retiring a warehouse nothing was ordered from: its stock, then it.
	a.data(`mutation { createWarehouse(input: {name: "South"}) { id } }`)
	a.data(`mutation { createStock(input: {quantity: 4, warehouseID: "2", productID: "1"}) { id } }`)
	a.refused(`mutation { deleteWarehouse(id: "2") }`, "FAILED_PRECONDITION", "still referenced")
	a.data(`mutation { deleteStock(id: "3") }`)
	if got := a.data(`mutation { deleteWarehouse(id: "2") }`); got != `{"deleteWarehouse":"2"}` {
		t.Errorf("deleteWarehouse = %s", got)
	}
	a.refused(`mutation { deleteStock(id: "3") }`, "NOT_FOUND", "not found")
}

// placeOrder is all or nothing: the order, its items at today's price, and
// the stock they come out of.
func TestPlaceOrderIsOneTransaction(t *testing.T) {
	a := start(t)
	a.shop(3, 1)

	placed := a.data(`mutation { placeOrder(input: {customerID: "1", warehouseID: "1", items: [
		{productID: "1", quantity: 2}, {productID: "2", quantity: 1}]}) {
		status totalCents items { quantity unitPriceCents product { sku } } } }`)
	want := `{"placeOrder":{"status":"PENDING","totalCents":11500,"items":[` +
		`{"quantity":2,"unitPriceCents":5000,"product":{"sku":"kb-1"}},` +
		`{"quantity":1,"unitPriceCents":1500,"product":{"sku":"kb-2"}}]}}`
	if placed != want {
		t.Fatalf("placeOrder = %s\nwant %s", placed, want)
	}
	stock := func() string { return a.data(`{ stocks { quantity } }`) }
	if got := stock(); got != `{"stocks":[{"quantity":1},{"quantity":0}]}` {
		t.Fatalf("stock after the order = %s", got)
	}

	// The first line fits (1 board left), the second does not (0 keycaps).
	// The whole order fails, and the board taken for the first line is put
	// back: no order, no items, stock unchanged.
	a.refused(`mutation { placeOrder(input: {customerID: "1", warehouseID: "1", items: [
		{productID: "1", quantity: 1}, {productID: "2", quantity: 1}]}) { id } }`, "FAILED_PRECONDITION", "items[1]: kb-2: not enough in stock")
	if got := stock(); got != `{"stocks":[{"quantity":1},{"quantity":0}]}` {
		t.Errorf("a failed order changed stock: %s", got)
	}
	if got := a.data(`{ orders { totalCount } orderItems { id } }`); got != `{"orders":{"totalCount":1},"orderItems":[{"id":"1"},{"id":"2"}]}` {
		t.Errorf("a failed order left rows behind: %s", got)
	}

	// Price is taken at order time: a later price change does not rewrite it.
	a.data(`mutation { updateProduct(id: "1", input: {priceCents: 9999}) { id } }`)
	if got := a.data(`{ order(id: "1") { totalCents } }`); got != `{"order":{"totalCents":11500}}` {
		t.Errorf("repricing a product rewrote a placed order: %s", got)
	}
}

// An order moves PENDING -> PAID -> SHIPPED, or to CANCELLED from either of
// the first two, and a move from the wrong state says what the state is.
// Cancelling puts the stock back.
func TestOrderLifecycle(t *testing.T) {
	a := start(t)
	a.shop(10, 10)
	a.data(placeTwoBoardsAndKeycaps) // 1
	a.data(placeTwoBoardsAndKeycaps) // 2

	a.refused(`mutation { shipOrder(id: "1") { status } }`, "FAILED_PRECONDITION", "order 1 is PENDING")
	if got := a.data(`mutation { payOrder(id: "1") { status } }`); got != `{"payOrder":{"status":"PAID"}}` {
		t.Errorf("payOrder = %s", got)
	}
	a.refused(`mutation { payOrder(id: "1") { status } }`, "FAILED_PRECONDITION", "order 1 is PAID")
	if got := a.data(`mutation { shipOrder(id: "1") { status } }`); got != `{"shipOrder":{"status":"SHIPPED"}}` {
		t.Errorf("shipOrder = %s", got)
	}
	a.refused(`mutation { cancelOrder(id: "1") { status } }`, "FAILED_PRECONDITION", "order 1 is SHIPPED")

	stock := `{ stocks { quantity } }`
	if got := a.data(stock); got != `{"stocks":[{"quantity":6},{"quantity":8}]}` {
		t.Fatalf("stock before cancelling = %s", got)
	}
	if got := a.data(`mutation { cancelOrder(id: "2") { status } }`); got != `{"cancelOrder":{"status":"CANCELLED"}}` {
		t.Errorf("cancelOrder = %s", got)
	}
	if got := a.data(stock); got != `{"stocks":[{"quantity":8},{"quantity":9}]}` {
		t.Errorf("cancelling did not return the stock: %s", got)
	}
	a.refused(`mutation { cancelOrder(id: "2") { status } }`, "FAILED_PRECONDITION", "order 2 is CANCELLED")
	a.refused(`mutation { payOrder(id: "999") { status } }`, "NOT_FOUND", "not found")
}

// Twenty orders race for five units: exactly five are placed, the other
// fifteen are refused as short, and the stock ends at zero. Take's
// conditional update is what decides; without its WHERE quantity >= n all
// twenty succeed and the stock ends at -15.
func TestConcurrentOrdersCannotOversell(t *testing.T) {
	a := start(t)
	a.shop(5, 0)

	var wg sync.WaitGroup
	var placed, short atomic.Int64
	for range 20 {
		wg.Go(func() {
			r := a.post(`mutation { placeOrder(input: {customerID: "1", warehouseID: "1", items: [{productID: "1", quantity: 1}]}) { id } }`)
			if len(r.Errors) == 0 {
				placed.Add(1)
			} else if strings.Contains(r.Errors[0].Message, "not enough in stock") {
				short.Add(1)
			}
		})
	}
	wg.Wait()

	var got struct {
		Stock  struct{ Quantity int }
		Orders struct{ TotalCount int }
	}
	if err := json.Unmarshal([]byte(a.data(`{ stock(id: "1") { quantity } orders { totalCount } }`)), &got); err != nil {
		t.Fatal(err)
	}
	if placed.Load() != 5 || short.Load() != 15 || got.Stock.Quantity != 0 || got.Orders.TotalCount != 5 {
		t.Fatalf("placed %d, short %d; stock %d, orders %d: want 5, 15; 0, 5", placed.Load(), short.Load(), got.Stock.Quantity, got.Orders.TotalCount)
	}
}

// A count moves by a delta, never below zero, and a warehouse holds one row
// per product.
func TestStockOnlyMovesByDelta(t *testing.T) {
	a := start(t)
	a.shop(3, 0)
	if got := a.data(`mutation { adjustStock(id: "1", delta: 4) { quantity } }`); got != `{"adjustStock":{"quantity":7}}` {
		t.Errorf("adjustStock(+4) = %s", got)
	}
	a.refused(`mutation { adjustStock(id: "1", delta: -8) { quantity } }`, "FAILED_PRECONDITION", "holds 7, cannot remove 8")
	if got := a.data(`mutation { adjustStock(id: "1", delta: -7) { quantity } }`); got != `{"adjustStock":{"quantity":0}}` {
		t.Errorf("adjustStock(-7) = %s", got)
	}
	a.refused(`mutation { createStock(input: {quantity: 1, warehouseID: "1", productID: "1"}) { id } }`, "CONFLICT", "already exists")
}

// A constraint velox enforces reaches the client as a GraphQL error on the
// field, not as a failed request.
func TestORMValidationIsAFieldError(t *testing.T) {
	a := start(t)
	a.refused(`mutation { createCategory(input: {name: ""}) { id } }`, "BAD_USER_INPUT", "name")
}

// A group left out of the app is a failed start that names what it left
// unbound, not a schema that builds and fails the first request to reach it.
// Product.stocks is how the catalog reaches the stock group.
func TestAMissingGroupFailsStart(t *testing.T) {
	// Every other group, from the one list resolvers.go registers from, so an
	// entity added there is in here too.
	a := fx.New(
		service.Module,
		fx.Options(resolverOptions("stock")...),
		appCore,
		fx.Supply(testConfig(t)),
		fx.NopLogger,
	)
	err := a.Err()
	if err == nil || !strings.Contains(err.Error(), "type Stock has no Object binding") {
		t.Fatalf("err = %v", err)
	}
	// And only stock: every unbound type it reports is Stock, once per field
	// that reaches it, and nothing else went missing with it.
	for _, m := range regexp.MustCompile(`type (\w+) has no Object binding`).FindAllStringSubmatch(err.Error(), -1) {
		if m[1] != "Stock" {
			t.Errorf("%s is unbound too; only stock was left out: %v", m[1], err)
		}
	}
}

// Stopping the app must release the port; a stop hook that returned before
// shutting the server down would leave it answering.
func TestStopClosesTheListener(t *testing.T) {
	var srv *Server
	a := fxtest.New(t,
		Module,
		fx.Supply(testConfig(t)),
		fx.Populate(&srv),
		fx.NopLogger,
	)
	a.RequireStart()
	url := "http://" + srv.Addr() + "/graphql"
	const q = `{"query":"{ categories { id } }"}`
	res, err := http.Post(url, "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	a.RequireStop()

	if _, err := http.Post(url, "application/json", strings.NewReader(q)); err == nil {
		t.Fatal("server still answering after stop")
	}
}

// BenchmarkNewSchema is the start-up cost of the layout: eight groups, each
// registered separately, merged into one schema.
//
//	go test -run '^$' -bench NewSchema
func BenchmarkNewSchema(b *testing.B) {
	for b.Loop() {
		if err := graph.ValidateSchema(scalars); err != nil {
			b.Fatal(err)
		}
	}
}

// A client that opens a connection and sends nothing -- a load balancer's
// warm pool, Go's Transport under concurrency -- holds up http.Server's
// Shutdown for five seconds, in case a request is on its way. With the stop
// bounded at five, that failed every stop; shutdownTimeout is longer.
func TestStopWaitsOutAnUnusedConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out http.Server's five-second grace")
	}
	var srv *Server
	a := fxtest.New(t,
		Module,
		fx.Supply(testConfig(t)),
		fx.Populate(&srv),
		fx.NopLogger,
	)
	a.RequireStart()
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	a.RequireStop()
	if d := time.Since(start); d >= shutdownTimeout {
		t.Errorf("stop took %v, the whole shutdownTimeout", d)
	}
}

// An error nothing classified -- here the driver's own, from a closed
// database -- reaches the client as INTERNAL_SERVER_ERROR with a fixed
// message, never with the driver's text, which is logged instead.
func TestUnclassifiedErrorsAreMasked(t *testing.T) {
	a := start(t, fx.Decorate(func(*velox.Client) (*velox.Client, error) {
		closed, err := openDB(t)
		if err != nil {
			return nil, err
		}
		return closed, closed.Close()
	}))
	r := a.post(`{ categories { name } }`)
	if len(r.Errors) != 1 || r.Errors[0].Extensions.Code != "INTERNAL_SERVER_ERROR" || r.Errors[0].Message != "internal error" {
		t.Fatalf("errors = %+v", r.Errors)
	}
}
