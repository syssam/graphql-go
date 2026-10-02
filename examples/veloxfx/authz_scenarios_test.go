package veloxfx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	graphql "github.com/syssam/graphql-go"
	categorysvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/category"
	customersvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/customer"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/service/order"
	productsvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/product"
	stocksvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/stock"
	warehousesvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	categoryclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/category"
	customerclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/customer"
	productclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/product"
	stockclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/stock"
	warehouseclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/warehouse"
	vorder "github.com/syssam/graphql-go/examples/veloxfx/velox/order"
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
// customers apart. A customer's own address is theirs to see, which is
// decided per row -- the authorization decision is made before any row
// exists. The column is still read -- the value is rewritten after it
// resolves -- so this is about what leaves the server, not what it loads.
func TestScenarioPersonalDataIsMasked(t *testing.T) {
	s := startRecording(t)
	s.storefront(0)
	const q = `{ customers { name email } }`

	got, _, _ := s.runAs("customer:1", q, nil)
	if !strings.Contains(got, `"email":"c1@example.com"`) {
		t.Errorf("a customer sees their own address: %s", got)
	}
	if strings.Contains(got, `c2@example.com`) || strings.Contains(got, `c3@example.com`) || strings.Count(got, `"email":"c*@example.com"`) != 2 {
		t.Errorf("without customer:pii everyone else's address is masked: %s", got)
	}
	got, _, _ = s.runAs("", q, nil)
	if strings.Count(got, `"email":"c*@example.com"`) != 3 {
		t.Errorf("nobody signed in: every address masked: %s", got)
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

// A customer can move only their own orders, and places orders only as
// themselves. Each refusal is checked against the database, not the
// response: the write this guards used to report NOT_FOUND and happen anyway.
func TestScenarioCustomersWriteOnlyTheirOwnOrders(t *testing.T) {
	a := start(t)
	a.seed() // orders 1-3, all customer 1's
	a.data(`mutation { createCustomer(input: {name: "Bob", email: "bob@example.com"}) { id } }`)
	status := func(id string) string {
		t.Helper()
		return a.data(fmt.Sprintf(`{ order(id: %q) { status } }`, id))
	}
	bob := a.as("customer:2")

	bob.refused(`mutation { payOrder(id: "1") { status } }`, "NOT_FOUND", "not found")
	bob.refused(`mutation { cancelOrder(id: "2") { status } }`, "NOT_FOUND", "not found")
	for _, id := range []string{"1", "2"} {
		if got := status(id); got != `{"order":{"status":"PENDING"}}` {
			t.Errorf("customer 2 moved customer 1's order %s: %s", id, got)
		}
	}

	stock := `{ stocks { quantity } }`
	before := a.data(stock)
	bob.refused(`mutation { placeOrder(input: {customerID: "1", warehouseID: "1", items: [{productID: "1", quantity: 1}]}) { id } }`,
		"FORBIDDEN", "cannot place an order for customer 1")
	if got := a.data(`{ orders { totalCount } }`); got != `{"orders":{"totalCount":3}}` {
		t.Errorf("customer 2 placed an order for customer 1: %s", got)
	}
	if got := a.data(stock); got != before {
		t.Errorf("a refused order moved stock: %s, was %s", got, before)
	}

	// The same operations on one's own orders go through.
	ada := a.as("customer:1")
	if got := ada.data(`mutation { payOrder(id: "1") { status } }`); got != `{"payOrder":{"status":"PAID"}}` {
		t.Errorf("customer 1 paying their own order = %s", got)
	}
	if got := ada.data(`mutation { cancelOrder(id: "2") { status } }`); got != `{"cancelOrder":{"status":"CANCELLED"}}` {
		t.Errorf("customer 1 cancelling their own order = %s", got)
	}
	ada.data(`mutation { placeOrder(input: {customerID: "1", warehouseID: "1", items: [{productID: "1", quantity: 1}]}) { id } }`)
}

// Everything but the customer's own order operations is staff's, and a
// refused mutation changes nothing.
func TestScenarioCatalogAndStockWritesAreStaffOnly(t *testing.T) {
	a := start(t)
	a.seed()
	before := a.data(`{ stocks { quantity } products(first: 10) { totalCount } }`)
	for viewer, code := range map[string]string{"": "UNAUTHENTICATED", "customer:1": "FORBIDDEN"} {
		as := a.as(viewer)
		as.refused(`mutation { createProduct(input: {sku: "x", name: "X", priceCents: 1, categoryID: "1"}) { id } }`, code, "createProduct")
		as.refused(`mutation { adjustStock(id: "1", delta: 100) { quantity } }`, code, "adjustStock")
		as.refused(`mutation { shipOrder(id: "1") { status } }`, code, "shipOrder")
		// A permitted field beside a forbidden one does not carry it through.
		as.refused(`mutation { payOrder(id: "1") { status } deleteOrder(id: "1") }`, code, "")
	}
	if got := a.data(`{ stocks { quantity } products(first: 10) { totalCount } }`); got != before {
		t.Errorf("a refused mutation changed the data: %s, was %s", got, before)
	}
	if got := a.data(`{ order(id: "1") { status } }`); got != `{"order":{"status":"PENDING"}}` {
		t.Errorf("a refused mutation moved order 1: %s", got)
	}
}

// mutationGate's own contract, over every mutation the schema serves: a new
// one is refused to customers until customerMutations names it. Driven
// directly, since reaching the gate end to end needs valid arguments for each.
func TestMutationGateRefusesEveryUnlistedMutation(t *testing.T) {
	a := start(t)
	var m struct {
		Type struct{ Fields []struct{ Name string } } `json:"__type"`
	}
	if err := json.Unmarshal([]byte(a.data(`{ __type(name: "Mutation") { fields { name } } }`)), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Type.Fields) < 10 {
		t.Fatalf("introspection found %d mutations; the gate would be checked against too few", len(m.Type.Fields))
	}
	gate := func(v viewer.Viewer, src string) (ran bool, code string) {
		t.Helper()
		doc, err := parser.ParseQuery(&ast.Source{Input: src})
		if err != nil {
			t.Fatal(err)
		}
		oc := &graphql.OperationContext{Operation: doc.Operations[0], Doc: doc}
		res := mutationGate.InterceptOperation(viewer.With(context.Background(), v), oc,
			func(context.Context, *graphql.OperationContext) *graphql.Response {
				ran = true
				return &graphql.Response{}
			})
		if len(res.Errors) > 0 {
			code, _ = res.Errors[0].Extensions["code"].(string)
		}
		return ran, code
	}
	customer := viewer.Viewer{CustomerID: 1}
	for _, f := range m.Type.Fields {
		src := "mutation { " + f.Name + " }"
		if ran, code := gate(viewer.Viewer{}, src); ran || code != "UNAUTHENTICATED" {
			t.Errorf("anonymous %s: ran=%v code=%q", f.Name, ran, code)
		}
		ran, code := gate(customer, src)
		if want := customerMutations[f.Name]; ran != want || (!want && code != "FORBIDDEN") {
			t.Errorf("customer %s: ran=%v code=%q, listed=%v", f.Name, ran, code, want)
		}
		if ran, _ := gate(viewer.Viewer{Staff: true}, src); !ran {
			t.Errorf("staff %s was refused", f.Name)
		}
	}
	// Nothing a document can do hides a field from the gate.
	for _, src := range []string{
		`mutation { ... on Mutation { adjustStock } }`,
		`mutation { ...F } fragment F on Mutation { adjustStock }`,
		`mutation($no: Boolean!) { adjustStock @skip(if: $no) }`,
		`mutation { payOrder ... on Mutation { ...F } } fragment F on Mutation { ...G } fragment G on Mutation { adjustStock ...F }`,
	} {
		if ran, code := gate(customer, src); ran || code != "FORBIDDEN" {
			t.Errorf("%s: ran=%v code=%q", src, ran, code)
		}
	}
	// Queries are not its business, and __typename is not a write.
	for _, src := range []string{`{ orders { totalCount } }`, `mutation { __typename payOrder }`} {
		if ran, _ := gate(customer, src); !ran {
			t.Errorf("%s was refused", src)
		}
	}
}

// The order service refuses an anonymous caller on its own, whoever calls
// it: mutationGate stops the same calls first over GraphQL, so no request
// can show that this half works.
func TestOrderServiceRefusesAnonymousWrites(t *testing.T) {
	ctx := context.Background()
	c, err := openDB(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	order.OwnOrders(c)
	cust := c.Customer.Create().SetName("Ada").SetEmail("ada@example.com").SaveX(ctx)
	wh := c.Warehouse.Create().SetName("North").SaveX(ctx)
	o := c.Order.Create().SetCustomerID(cust.ID).SetWarehouseID(wh.ID).SaveX(ctx)
	svc := order.New(c)
	anon := viewer.With(ctx, viewer.Viewer{})
	code := func(err error) string {
		var gerr *graphql.Error
		if errors.As(err, &gerr) {
			s, _ := gerr.Extensions["code"].(string)
			return s
		}
		return fmt.Sprint(err)
	}

	if _, err := svc.Place(anon, order.PlaceInput{CustomerID: 0, WarehouseID: wh.ID, Lines: []order.Line{{ProductID: 1, Quantity: 1}}}); code(err) != "UNAUTHENTICATED" {
		t.Errorf("anonymous Place: %v", err)
	}
	if _, err := svc.Pay(anon, o.ID); code(err) != "UNAUTHENTICATED" {
		t.Errorf("anonymous Pay: %v", err)
	}
	if _, err := svc.Cancel(anon, o.ID); code(err) != "UNAUTHENTICATED" {
		t.Errorf("anonymous Cancel: %v", err)
	}
	staff := viewer.With(ctx, viewer.Viewer{Staff: true})
	if n, err := c.Order.Query().Count(staff); err != nil || n != 1 {
		t.Errorf("anonymous writes left %d orders (err %v), want the one", n, err)
	}
	if got := c.Order.GetX(staff, o.ID).Status; got != vorder.StatusPENDING {
		t.Errorf("anonymous writes moved the order to %s", got)
	}
}

// Every write only staff may make is refused by its service on its own,
// whoever calls it: mutationGate refuses the same calls first over GraphQL,
// so no request can show that the services do. Each call is made as a
// customer and as nobody, and the data must be as it was.
func TestServicesRefuseNonStaffWrites(t *testing.T) {
	ctx := context.Background()
	c, err := openDB(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	cat := c.Category.Create().SetName("Keyboards").SaveX(ctx)
	prod := c.Product.Create().SetSku("kb-1").SetName("Board").SetPriceCents(5000).SetCategoryID(cat.ID).SaveX(ctx)
	cust := c.Customer.Create().SetName("Ada").SetEmail("ada@example.com").SaveX(ctx)
	wh := c.Warehouse.Create().SetName("North").SaveX(ctx)
	st := c.Stock.Create().SetWarehouseID(wh.ID).SetProductID(prod.ID).SetQuantity(5).SaveX(ctx)
	o := c.Order.Create().SetCustomerID(cust.ID).SetWarehouseID(wh.ID).SetStatus(vorder.StatusPAID).SaveX(ctx)

	cats, prods, custs, whs, stocks := categorysvc.New(c), productsvc.New(c), customersvc.New(c), warehousesvc.New(c), stocksvc.New(c)
	orders := order.New(c)
	calls := map[string]func(context.Context) error{
		"category.Create": func(ctx context.Context) error {
			_, err := cats.Create(ctx, categoryclient.CreateCategoryInput{Name: "x"})
			return err
		},
		"category.Update": func(ctx context.Context) error {
			_, err := cats.Update(ctx, cat.ID, categoryclient.UpdateCategoryInput{})
			return err
		},
		"category.Delete": func(ctx context.Context) error { return cats.Delete(ctx, cat.ID) },
		"product.Create": func(ctx context.Context) error {
			_, err := prods.Create(ctx, productclient.CreateProductInput{})
			return err
		},
		"product.Update": func(ctx context.Context) error {
			_, err := prods.Update(ctx, prod.ID, productclient.UpdateProductInput{})
			return err
		},
		"product.Delete": func(ctx context.Context) error { return prods.Delete(ctx, prod.ID) },
		"customer.Create": func(ctx context.Context) error {
			_, err := custs.Create(ctx, customerclient.CreateCustomerInput{})
			return err
		},
		"customer.Update": func(ctx context.Context) error {
			_, err := custs.Update(ctx, cust.ID, customerclient.UpdateCustomerInput{})
			return err
		},
		"customer.Delete": func(ctx context.Context) error { return custs.Delete(ctx, cust.ID) },
		"warehouse.Create": func(ctx context.Context) error {
			_, err := whs.Create(ctx, warehouseclient.CreateWarehouseInput{})
			return err
		},
		"warehouse.Update": func(ctx context.Context) error {
			_, err := whs.Update(ctx, wh.ID, warehouseclient.UpdateWarehouseInput{})
			return err
		},
		"warehouse.Delete": func(ctx context.Context) error { return whs.Delete(ctx, wh.ID) },
		"stock.Create": func(ctx context.Context) error {
			_, err := stocks.Create(ctx, stockclient.CreateStockInput{})
			return err
		},
		"stock.Adjust": func(ctx context.Context) error { _, err := stocks.Adjust(ctx, st.ID, 100); return err },
		"stock.Delete": func(ctx context.Context) error { return stocks.Delete(ctx, st.ID) },
		"order.Ship":   func(ctx context.Context) error { _, err := orders.Ship(ctx, o.ID); return err },
		"order.Delete": func(ctx context.Context) error { return orders.Delete(ctx, o.ID) },
	}
	code := func(err error) string {
		var gerr *graphql.Error
		if errors.As(err, &gerr) {
			s, _ := gerr.Extensions["code"].(string)
			return s
		}
		return fmt.Sprint(err)
	}
	for who, want := range map[viewer.Viewer]string{{}: "UNAUTHENTICATED", {CustomerID: cust.ID}: "FORBIDDEN"} {
		vctx := viewer.With(ctx, who)
		for name, call := range calls {
			if got := code(call(vctx)); got != want {
				t.Errorf("%s as %+v: %s, want %s", name, who, got, want)
			}
		}
	}

	staff := viewer.With(ctx, viewer.Viewer{Staff: true})
	for name, n := range map[string]func() (int, error){
		"categories": func() (int, error) { return c.Category.Query().Count(staff) },
		"products":   func() (int, error) { return c.Product.Query().Count(staff) },
		"customers":  func() (int, error) { return c.Customer.Query().Count(staff) },
		"warehouses": func() (int, error) { return c.Warehouse.Query().Count(staff) },
		"stocks":     func() (int, error) { return c.Stock.Query().Count(staff) },
		"orders":     func() (int, error) { return c.Order.Query().Count(staff) },
	} {
		if got, err := n(); err != nil || got != 1 {
			t.Errorf("%s: %d (err %v) after refused writes, want 1", name, got, err)
		}
	}
	if got := c.Stock.GetX(staff, st.ID).Quantity; got != 5 {
		t.Errorf("stock quantity %d after refused writes, want 5", got)
	}
	if got := c.Order.GetX(staff, o.ID).Status; got != vorder.StatusPAID {
		t.Errorf("order moved to %s by a refused write", got)
	}
}

// Every way to read a row with a rule on it, as every kind of viewer: each
// root, lookup and edge that reaches a Stock, an Order or an OrderItem. A rule
// that holds only on the path someone tested is how order lines and stock
// levels stayed readable by anyone after orders were narrowed; a path added
// to the schema later belongs in this table.
//
// A number is how many rows the viewer sees; unauthenticated means the read
// fails closed with that code. Ada owns all three orders and their six lines;
// Bob is signed in and owns none.
func TestEveryReadPathKeepsThePolicy(t *testing.T) {
	a := start(t)
	a.seed()
	a.data(`mutation { createCustomer(input: {name: "Bob", email: "bob@example.com"}) { id } }`)
	const unauthenticated = -1
	type seen struct{ staff, owner, other, anonymous int }
	for _, c := range []struct {
		query string
		want  seen
	}{
		// Stock levels: inventory:read, which only staff hold.
		{`{ stocks { id } }`, seen{2, 0, 0, 0}},
		{`{ stock(id: "1") { id } }`, seen{1, 0, 0, 0}},
		{`{ warehouses { stocks { id } } }`, seen{2, 0, 0, 0}},
		{`{ warehouse(id: "1") { stocks { id } } }`, seen{2, 0, 0, 0}},
		{`{ products(first: 10) { edges { node { stocks { id } } } } }`, seen{2, 0, 0, 0}},
		// Orders: their customer's, and staff's.
		{`{ orders(first: 50) { edges { node { id } } } }`, seen{3, 3, 0, unauthenticated}},
		{`{ order(id: "1") { id } }`, seen{1, 1, 0, unauthenticated}},
		{`{ customer(id: "1") { orders(first: 50) { edges { node { id } } } } }`, seen{3, 3, 0, unauthenticated}},
		{`{ warehouse(id: "1") { orders(first: 50) { edges { node { id } } } } }`, seen{3, 3, 0, unauthenticated}},
		// Order lines: their order's customer's, and staff's.
		{`{ orderItems { id } }`, seen{6, 6, 0, unauthenticated}},
		{`{ orderItem(id: "1") { id } }`, seen{1, 1, 0, unauthenticated}},
		{`{ product(id: "1") { orderItems { id } } }`, seen{3, 3, 0, unauthenticated}},
		{`{ order(id: "1") { items { id } } }`, seen{2, 2, 0, unauthenticated}},
	} {
		for who, want := range map[string]int{"staff": c.want.staff, "customer:1": c.want.owner, "customer:2": c.want.other, "": c.want.anonymous} {
			r := a.as(who).post(c.query)
			var codes []string
			for _, e := range r.Errors {
				codes = append(codes, e.Extensions.Code)
			}
			got := strings.Count(string(r.Data), `"id":`)
			switch {
			case want == unauthenticated:
				if got != 0 || len(codes) != 1 || codes[0] != "UNAUTHENTICATED" {
					t.Errorf("%s as %q: want UNAUTHENTICATED and no rows, got %d rows, errors %v (data %s)", c.query, who, got, codes, r.Data)
				}
			case len(codes) > 0 || got != want:
				t.Errorf("%s as %q: want %d rows, got %d, errors %v (data %s)", c.query, who, want, got, codes, r.Data)
			}
		}
	}
}

// The gate expands each named fragment once per operation however many times
// it is spread, and through however many inline fragments: otherwise a small
// document repeating one spread makes the gate's walk grow before any limit
// applies.
func TestMutationGateExpandsEachFragmentOnce(t *testing.T) {
	doc, err := parser.ParseQuery(&ast.Source{Input: `mutation {
		... on Mutation { ...A } ... on Mutation { ...A } ... on Mutation { ... on Mutation { ...A } } ...A
	} fragment A on Mutation { adjustStock }`})
	if err != nil {
		t.Fatal(err)
	}
	if got := operationRootFields(doc, doc.Operations[0]); len(got) != 1 {
		t.Errorf("rootFields = %v: fragment A expanded %d times, want once", got, len(got))
	}
}
