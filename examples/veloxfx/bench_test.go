package veloxfx

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
)

// benchApp is the whole app minus HTTP: the executor the server would call,
// over a database seeded with a storefront of orders orders.
func benchApp(b *testing.B, orders int, extra ...graphql.ExecutorOption) (*graphql.Executor, context.Context) {
	b.Helper()
	var (
		exec   *graphql.Executor
		schema *graphql.Schema
	)
	a := fxtest.New(b, Module, fx.Populate(&schema),
		fx.Supply(Config{Addr: "127.0.0.1:0", DSN: "file:" + b.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"}),
		fx.Populate(&exec), fx.NopLogger)
	a.RequireStart()
	b.Cleanup(a.RequireStop)
	ctx := viewer.With(context.Background(), viewer.Viewer{Staff: true})
	do := func(q string) {
		if r := exec.Execute(ctx, &graphql.Request{Query: q}); len(r.Errors) > 0 {
			b.Fatalf("%s: %v", q, r.Errors)
		}
	}
	for c := 1; c <= 3; c++ {
		do(fmt.Sprintf(`mutation { createCategory(input: {name: "cat-%d"}) { id } }`, c))
	}
	do(`mutation { createWarehouse(input: {name: "North"}) { id } }`)
	for p := 1; p <= 12; p++ {
		do(fmt.Sprintf(`mutation { createProduct(input: {sku: "sku-%02d", name: "product %d", priceCents: %d, categoryID: "%d"}) { id } }`, p, p, p*100, (p-1)/4+1))
		do(fmt.Sprintf(`mutation { createStock(input: {quantity: 100000, warehouseID: "1", productID: "%d"}) { id } }`, p))
	}
	for c := 1; c <= 3; c++ {
		do(fmt.Sprintf(`mutation { createCustomer(input: {name: "customer %d", email: "c%d@example.com"}) { id } }`, c, c))
	}
	for o := range orders {
		do(fmt.Sprintf(`mutation { placeOrder(input: {customerID: "%d", warehouseID: "1",
			items: [{productID: "%d", quantity: 2}, {productID: "%d", quantity: 1}]}) { id } }`, o%3+1, o%12+1, (o+5)%12+1))
	}
	if len(extra) > 0 {
		exec = graphql.NewExecutor(schema, append(executorOptions(), extra...)...)
	}
	return exec, ctx
}

// BenchmarkOrderHistoryPage is the Relay order-history page of
// scenarios_test.go, fifty orders deep, as one request costs end to end:
// planning (cached after the first), collection, four SQL queries, and the
// JSON written.
//
// "concurrent" is the app as configured; "inline" runs every field in one
// goroutine and is the ceiling. They were 1.46ms and ~0.7ms until gqlc.yaml's
// inlineAccessors and inline kept a goroutine per order from being started
// for fields that read loaded edges; concurrent is now 0.82ms.
//
//	go test -run '^$' -bench OrderHistoryPage -benchmem
func BenchmarkOrderHistoryPage(b *testing.B) {
	b.Run("concurrent", func(b *testing.B) { benchOrderHistory(b) })
	b.Run("inline", func(b *testing.B) { benchOrderHistory(b, graphql.WithMaxConcurrency(0)) })
}

func benchOrderHistory(b *testing.B, extra ...graphql.ExecutorOption) {
	exec, ctx := benchApp(b, 50, extra...)
	vars, _ := json.Marshal(map[string]any{"first": 50})
	req := &graphql.Request{Query: `
		query OrderHistory($first: Int!) {
			orders(first: $first) { edges { node { id ...OrderRow_order } } pageInfo { hasNextPage endCursor } }
		}
		fragment OrderRow_order on Order { status totalCents customer { ...CustomerBadge_customer } items { ...LineItem_item } }
		fragment CustomerBadge_customer on Customer { name }
		fragment LineItem_item on OrderItem { quantity product { sku } }`, Variables: vars}
	b.ReportAllocs()
	for b.Loop() {
		if r := exec.Execute(ctx, req); len(r.Errors) > 0 {
			b.Fatal(r.Errors)
		}
	}
}
