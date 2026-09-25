// Package storefront is a back-office API written the way a deployment has
// to write one: every position says what it requires, and nothing is visible
// because nobody thought to ask for it.
//
// What it is here to show, and what the other examples do not:
//
//   - authorization declared in the SDL and enforced by the engine --
//     @requiresScopes, a marker directive, an argument site and per-row
//     instance checks -- rather than by a middleware that has to be
//     remembered at every call site;
//   - RequireAuthCoverage, so a field added without a decision fails the
//     build instead of shipping open;
//   - the four withholding outcomes used where each is right: Allow, Redact,
//     Deny and Drop;
//   - a DataLoader whose batching survives the row checks above it.
//
// The server wiring -- limits, persisted queries, tracing, a shutdown drain --
// is in cmd/server, so that this file stays about the schema.
package storefront

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

//go:embed schema.graphql
var sdl string

// New returns the schema and the store behind it. The store is returned
// rather than hidden because a caller has to be able to place an order to
// see a subscription fire, and because the tests drive both.
func New(opts ...graphql.SchemaOption) (*graphql.Schema, *Store, error) {
	store := NewStore()
	s, err := NewSchema(store, opts...)
	return s, store, err
}

// NewSchema builds the schema over an existing store.
func NewSchema(store *Store, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	return newSchemaFrom(store, graphql.SDL(sdl), opts...)
}

// newSchemaFrom takes the SDL as a parameter so the test for
// RequireAuthCoverage can add a field to it. Nothing else has a reason to
// build this schema over different text.
func newSchemaFrom(store *Store, src graphql.Source, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	customers := newCustomerLoader(store)

	all := []graphql.SchemaOption{
		// RequireAuthCoverage is the reason the SDL carries @public on
		// Query.health: with it on, a field that declares neither a
		// requirement nor @public fails NewSchema. What it catches is not a
		// field someone forgot to guard -- those get noticed -- but a type
		// nobody noticed arriving.
		graphql.RequireAuthCoverage(),

		// @authenticated has no argument: its presence is a requirement for
		// one scope, and what that scope means is the Authorizer's business.
		// It composes with @requiresScopes rather than replacing it, so
		// Mutation.refundOrder needs order:write and Order needs
		// authenticated, and a caller must hold both.
		graphql.MarkerDirective("authenticated", ScopeAuthenticated),

		graphql.Scalar("Money", marshalMoney, unmarshalMoney),
		graphql.Time("Time"),

		graphql.Enum("OrderStatus", map[OrderStatus]string{
			StatusPending:  "PENDING",
			StatusPaid:     "PAID",
			StatusShipped:  "SHIPPED",
			StatusRefunded: "REFUNDED",
		}),

		graphql.Input[OrderLine]("OrderLineInput",
			graphql.InputField("sku", func(l *OrderLine, v string) { l.SKU = v }),
			graphql.InputField("description", func(l *OrderLine, v string) { l.Description = v }),
			graphql.InputField("quantity", func(l *OrderLine, v int) { l.Quantity = v }),
			graphql.InputField("unitPrice", func(l *OrderLine, v Money) { l.UnitPrice = v }),
		),
		graphql.Input[PlaceOrder]("PlaceOrderInput",
			graphql.InputField("customerId", func(p *PlaceOrder, v *graphql.ID) { p.CustomerID = v }),
			graphql.InputField("lines", func(p *PlaceOrder, v []OrderLine) { p.Lines = v }),
		),

		graphql.Input[OrderFilter]("OrderWhere",
			graphql.InputField("status", func(f *OrderFilter, v *OrderStatus) { f.Status = v }),
			graphql.InputField("minMargin", func(f *OrderFilter, v *Money) { f.MinMargin = v }),
		),

		graphql.Object[Customer]("Customer",
			graphql.Field("id", func(c *Customer) graphql.ID { return graphql.ID(c.ID) }),
			graphql.Field("name", func(c *Customer) string { return c.Name }),
			graphql.Field("email", func(c *Customer) string { return c.Email }),
			graphql.Resolve("orders", func(_ context.Context, c *Customer) ([]*Order, error) {
				return store.OrdersOf(c.ID), nil
			}),
		),

		graphql.Object[Order]("Order",
			graphql.Field("id", func(o *Order) graphql.ID { return graphql.ID(o.ID) }),
			graphql.Field("reference", func(o *Order) string { return o.Reference }),
			graphql.Field("status", func(o *Order) OrderStatus { return o.Status }),
			graphql.Field("placedAt", func(o *Order) time.Time { return o.PlacedAt }),
			graphql.Field("total", func(o *Order) Money { return o.Total }),
			graphql.Field("margin", func(o *Order) Money { return o.Margin }),
			graphql.Field("lines", func(o *Order) []OrderLine { return o.Lines }),
			// The one field that does I/O per order, and so the one the
			// loader exists for. A listing of n orders belonging to two
			// customers fetches two customers, not n.
			graphql.Resolve("customer", func(ctx context.Context, o *Order) (*Customer, error) {
				return customers.Load(ctx, o.CustomerID)
			}),
		),

		graphql.Object[OrderLine]("OrderLine",
			graphql.Field("sku", func(l *OrderLine) string { return l.SKU }),
			graphql.Field("description", func(l *OrderLine) string { return l.Description }),
			graphql.Field("quantity", func(l *OrderLine) int { return l.Quantity }),
			graphql.Field("unitPrice", func(l *OrderLine) Money { return l.UnitPrice }),
		),

		graphql.Args[ordersArgs](
			graphql.InputField("where", func(a *ordersArgs, v *OrderFilter) { a.Where = v }),
			graphql.InputField("first", func(a *ordersArgs, v int) { a.First = v }),
		),
		graphql.Args[idArgs](
			graphql.InputField("id", func(a *idArgs, v graphql.ID) { a.ID = v }),
		),
		graphql.Args[refundArgs](
			graphql.InputField("id", func(a *refundArgs, v graphql.ID) { a.ID = v }),
			graphql.InputField("reason", func(a *refundArgs, v string) { a.Reason = v }),
		),
		graphql.Args[placeArgs](
			graphql.InputField("input", func(a *placeArgs, v PlaceOrder) { a.Input = v }),
		),

		graphql.Query(
			graphql.Field("health", func(graphql.Root) string { return "ok" }),
			graphql.ResolveArgs("orders", func(_ context.Context, _ graphql.Root, a ordersArgs) ([]*Order, error) {
				return store.Orders(a.Where, a.First), nil
			}),
			graphql.ResolveArgs("order", func(_ context.Context, _ graphql.Root, a idArgs) (*Order, error) {
				return store.Order(string(a.ID)), nil
			}),
			graphql.ResolveArgs("customer", func(ctx context.Context, _ graphql.Root, a idArgs) (*Customer, error) {
				return customers.Load(ctx, string(a.ID))
			}),
		),

		graphql.Mutation(
			// The policy has already refused a customerId this principal may
			// not supply, so by the time this runs an absent one means "for
			// myself" and a present one has been allowed.
			graphql.ResolveArgs("placeOrder", func(ctx context.Context, _ graphql.Root, a placeArgs) (*Order, error) {
				id := PrincipalFrom(ctx).CustomerID
				if a.Input.CustomerID != nil {
					id = string(*a.Input.CustomerID)
				}
				if id == "" {
					return nil, errors.New("no customer to place the order for; supply input.customerId")
				}
				return store.Place(id, a.Input.Lines)
			}),
			graphql.ResolveArgs("refundOrder", func(_ context.Context, _ graphql.Root, a refundArgs) (*Order, error) {
				return store.Refund(string(a.ID))
			}),
		),

		graphql.Subscription(
			graphql.Subscribe("orderPlaced", func(ctx context.Context) (<-chan *Order, error) {
				return store.OrdersPlaced(ctx), nil
			}),
		),
	}
	return graphql.NewSchema(src, append(all, opts...)...)
}

// ExecutorOptions is the authorization half of the executor's configuration:
// the two policies, and nothing else. cmd/server adds the limits, the
// persisted-query cache and the tracing on top; the tests use these alone, so
// that a failing test names an authorization bug and not a limit.
//
// Both are needed. WithAuthorizer alone leaves @authorizeObject describing
// positions that nothing enforces, which is worse than not declaring it.
func ExecutorOptions() []graphql.ExecutorOption {
	var p Policy
	return []graphql.ExecutorOption{
		graphql.WithAuthorizer(p),
		graphql.WithObjectAuthorizer(p),
	}
}

type ordersArgs struct {
	Where *OrderFilter
	First int
}

type idArgs struct{ ID graphql.ID }

// PlaceOrder is the decoded PlaceOrderInput. CustomerID is a pointer because
// the SDL position is nullable and omitting it is the whole signal: a
// customer ordering for themselves does not name themselves.
type PlaceOrder struct {
	CustomerID *graphql.ID
	Lines      []OrderLine
}

type placeArgs struct{ Input PlaceOrder }

type refundArgs struct {
	ID     graphql.ID
	Reason string
}

// newCustomerLoader batches customer reads within one concurrent wave: a
// listing of n orders belonging to two customers fetches two customers, not
// n. A key the batch leaves out of the map is a not-found, which Load reports
// as the zero value and a nil error -- which is what Order.customer wants,
// since a nil there is a dangling reference and not a failure to fetch.
func newCustomerLoader(store *Store) *loader.Loader[string, *Customer] {
	return loader.New(func(_ context.Context, ids []string) (map[string]*Customer, error) {
		found := store.Customers(ids)
		out := make(map[string]*Customer, len(ids))
		for i, c := range found {
			if c != nil {
				out[ids[i]] = c
			}
		}
		return out, nil
	})
}

func marshalMoney(w *graphql.Writer, m Money) error {
	w.String(m.String())
	return nil
}

func unmarshalMoney(v any) (Money, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("Money must be a decimal string such as \"12.34\", got %T", v)
	}
	return ParseMoney(s)
}
