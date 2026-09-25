package veloxfx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/syssam/graphql-go/examples/veloxfx/graph"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/catalog"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/sales"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
)

// The generated bindings must match the SDL velox wrote, and a clean build
// says nothing about that; ValidateSchema does, with no database behind it.
func TestSchemaBinds(t *testing.T) {
	if err := graph.ValidateSchema(scalars); err != nil {
		t.Fatal(err)
	}
}

// Every entity is its own group, and no root field stayed in root: the ones
// velox declares follow the type they return, and deleteProduct, which
// returns an ID, follows the file it is declared in, sdl/product.graphql.
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
	if !strings.Contains(read("product"), "DeleteProduct(ctx") {
		t.Error("deleteProduct is not in the product group")
	}
	if !strings.Contains(read("order"), "OrderTotalCents(ctx") {
		t.Error("Order.totalCents is not in the order group")
	}
}

type app struct {
	t   *testing.T
	url string
}

// start runs the whole Module on a free port against a database no other
// test shares, and stops it when the test ends.
func start(t *testing.T, opts ...fx.Option) *app {
	t.Helper()
	var srv *Server
	a := fxtest.New(t, append([]fx.Option{
		Module,
		fx.Supply(Config{Addr: "127.0.0.1:0", DSN: dsn(t)}),
		fx.Populate(&srv),
		fx.NopLogger,
	}, opts...)...)
	a.RequireStart()
	t.Cleanup(a.RequireStop)
	return &app{t: t, url: "http://" + srv.Addr() + "/graphql"}
}

func dsn(t *testing.T) string {
	return "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"
}

type response struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (a *app) post(query string) response {
	a.t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		a.t.Fatal(err)
	}
	res, err := http.Post(a.url, "application/json", bytes.NewReader(body))
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

func (a *app) data(query string) string {
	a.t.Helper()
	r := a.post(query)
	if len(r.Errors) > 0 {
		a.t.Fatalf("%s: %+v", query, r.Errors)
	}
	return string(r.Data)
}

// seed builds a small shop through the API: two products in one category,
// stocked in one warehouse, and three orders of two items each.
func (a *app) seed() {
	a.t.Helper()
	a.data(`mutation { createCategory(input: {name: "Keyboards"}) { id } }`)
	a.data(`mutation { createProduct(input: {sku: "kb-1", name: "Board", priceCents: 5000, categoryID: "1"}) { id } }`)
	a.data(`mutation { createProduct(input: {sku: "kb-2", name: "Keycaps", priceCents: 1500, categoryID: "1"}) { id } }`)
	a.data(`mutation { createWarehouse(input: {name: "North"}) { id } }`)
	a.data(`mutation { createStock(input: {quantity: 7, warehouseID: "1", productID: "1"}) { id } }`)
	a.data(`mutation { createCustomer(input: {name: "Ada", email: "ada@example.com"}) { id } }`)
	for o := 1; o <= 3; o++ {
		a.data(`mutation { createOrder(input: {customerID: "1"}) { id } }`)
		a.data(fmt.Sprintf(`mutation { createOrderItem(input: {quantity: 2, unitPriceCents: 5000, orderID: "%d", productID: "1"}) { id } }`, o))
		a.data(fmt.Sprintf(`mutation { createOrderItem(input: {quantity: 1, unitPriceCents: 1500, orderID: "%d", productID: "2"}) { id } }`, o))
	}
}

func TestServesThroughEveryDomain(t *testing.T) {
	a := start(t)
	a.seed()

	// Created rows are read back, so edges answer with real rows rather than
	// velox's id-only stubs: a stub would say "name":"".
	got := a.data(`mutation { createProduct(input: {sku: "kb-3", name: "Switches", priceCents: 900, categoryID: "1"}) { sku category { name } } }`)
	if want := `{"createProduct":{"sku":"kb-3","category":{"name":"Keyboards"}}}`; got != want {
		t.Errorf("createProduct = %s, want %s", got, want)
	}

	// sales -> catalog, and the computed totalCents from sdl/order.graphql.
	got = a.data(`{ orders { status totalCents customer { name } items { quantity product { sku category { name } } } } }`)
	order := `{"status":"PENDING","totalCents":11500,"customer":{"name":"Ada"},"items":[` +
		`{"quantity":2,"product":{"sku":"kb-1","category":{"name":"Keyboards"}}},` +
		`{"quantity":1,"product":{"sku":"kb-2","category":{"name":"Keyboards"}}}]}`
	if want := `{"orders":[` + order + `,` + order + `,` + order + `]}`; got != want {
		t.Errorf("orders = %s", got)
	}

	// inventory -> catalog.
	got = a.data(`{ warehouses { name stocks { quantity product { name } } } }`)
	if want := `{"warehouses":[{"name":"North","stocks":[{"quantity":7,"product":{"name":"Board"}}]}]}`; got != want {
		t.Errorf("warehouses = %s", got)
	}

	got = a.data(`mutation { updateOrder(id: "2", input: {status: PAID}) { status } }`)
	if want := `{"updateOrder":{"status":"PAID"}}`; got != want {
		t.Errorf("updateOrder = %s", got)
	}

	// deleteProduct is hand-written SDL returning a scalar. A product nothing
	// references deletes; one an order item references is refused by the
	// foreign key, and the refusal is a field error, not a failed request.
	if got := a.data(`mutation { deleteProduct(id: "3") }`); got != `{"deleteProduct":"3"}` {
		t.Errorf("deleteProduct(3) = %s", got)
	}
	if r := a.post(`mutation { deleteProduct(id: "1") }`); len(r.Errors) != 1 {
		t.Errorf("deleting a referenced product: errors = %+v, data = %s", r.Errors, r.Data)
	}
}

// Every list root eager-loads what its type exposes, across domains, so one
// request is a fixed number of queries however many rows it returns. Three
// orders of two items each, loaded one edge at a time, is 1 + 3 customers +
// 3 item lists + 6 products = 13 queries; loaded eagerly it is 4, and
// totalCents reuses the items already loaded.
func TestOrdersAreAFixedNumberOfQueries(t *testing.T) {
	var queries, counting atomic.Int64
	a := start(t, fx.Decorate(func(*velox.Client) (*velox.Client, error) {
		counted, err := velox.Open("sqlite", dsn(t), velox.Debug(), velox.Log(func(v ...any) {
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
	a.data(`{ orders { totalCents customer { name } items { product { name } } } }`)
	counting.Store(0)
	if n := queries.Load(); n != 4 {
		t.Errorf("orders ran %d queries, want 4 (orders, customers, items, products)", n)
	}
}

// A constraint velox enforces reaches the client as a GraphQL error on the
// field, not as a failed request.
func TestORMValidationIsAFieldError(t *testing.T) {
	a := start(t)
	r := a.post(`mutation { createCategory(input: {name: ""}) { id } }`)
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Message, "name") {
		t.Fatalf("want one error naming the field, got %+v (data %s)", r.Errors, r.Data)
	}
}

// A domain module left out of the app is a failed start that names what it
// left unbound, not a schema that builds and fails the first request to
// reach it. catalog depends on inventory through Product.stocks.
func TestAMissingDomainFailsStart(t *testing.T) {
	a := fx.New(
		catalog.Module, sales.Module, // inventory.Module left out
		appCore,
		fx.Supply(Config{Addr: "127.0.0.1:0", DSN: dsn(t)}),
		fx.NopLogger,
	)
	err := a.Err()
	if err == nil || !strings.Contains(err.Error(), "type Stock has no Object binding") {
		t.Fatalf("err = %v", err)
	}
}

// Stopping the app must release the port; a stop hook that returned before
// shutting the server down would leave it answering.
func TestStopClosesTheListener(t *testing.T) {
	var srv *Server
	a := fxtest.New(t,
		Module,
		fx.Supply(Config{Addr: "127.0.0.1:0", DSN: dsn(t)}),
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
