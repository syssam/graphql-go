// Package sales implements the customer, order and orderitem groups.
package sales

import (
	"context"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	customergql "github.com/syssam/graphql-go/examples/veloxfx/graph/customer"
	ordergql "github.com/syssam/graphql-go/examples/veloxfx/graph/order"
	orderitemgql "github.com/syssam/graphql-go/examples/veloxfx/graph/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Module provides the domain's resolvers and registers their bindings.
var Module = fx.Module("sales",
	fx.Provide(NewCustomerResolver, NewOrderResolver, NewOrderItemResolver),
	resolve.Bindings(func(r *CustomerResolver) graphql.SchemaOption { return customergql.Bindings(r) }),
	resolve.Bindings(func(r *OrderResolver) graphql.SchemaOption { return ordergql.Bindings(r) }),
	resolve.Bindings(func(r *OrderItemResolver) graphql.SchemaOption { return orderitemgql.Bindings(r) }),
)

type CustomerResolver struct{ client *velox.Client }

var _ customergql.Resolver = (*CustomerResolver)(nil)

func NewCustomerResolver(client *velox.Client) *CustomerResolver {
	return &CustomerResolver{client: client}
}

func (r *CustomerResolver) Customers(ctx context.Context) ([]*entity.Customer, error) {
	return resolve.List(r.client.Customer.Query().WithOrders().All(ctx))
}

func (r *CustomerResolver) CreateCustomer(ctx context.Context, args customergql.CreateCustomerArgs) (*entity.Customer, error) {
	return r.client.Customer.Create().SetInput(args.Input).Save(ctx)
}

func (r *CustomerResolver) CustomerID(_ context.Context, c *entity.Customer) (graphql.ID, error) {
	return resolve.ID(c.ID), nil
}

type OrderResolver struct{ client *velox.Client }

var _ ordergql.Resolver = (*OrderResolver)(nil)

func NewOrderResolver(client *velox.Client) *OrderResolver {
	return &OrderResolver{client: client}
}

// Orders loads two levels, and one of them crosses into the catalog domain:
// orders { items { product { name } } } is three queries, not one per item.
func (r *OrderResolver) Orders(ctx context.Context) ([]*entity.Order, error) {
	return resolve.List(r.client.Order.Query().
		WithCustomer().
		WithItems(func(q entity.OrderItemQuerier) { q.WithProduct() }).
		All(ctx))
}

func (r *OrderResolver) CreateOrder(ctx context.Context, args ordergql.CreateOrderArgs) (*entity.Order, error) {
	o, err := r.client.Order.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, o.ID)
}

func (r *OrderResolver) UpdateOrder(ctx context.Context, args ordergql.UpdateOrderArgs) (*entity.Order, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Order.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

// OrderTotalCents answers Order.totalCents, which sdl/order.graphql adds and
// velox knows nothing about. Items come from the edge, so under Orders it
// reads what was eager-loaded and under a single order it queries once.
func (r *OrderResolver) OrderTotalCents(ctx context.Context, o *entity.Order) (int, error) {
	items, err := o.Items(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, it := range items {
		total += it.Quantity * it.UnitPriceCents
	}
	return total, nil
}

func (r *OrderResolver) OrderID(_ context.Context, o *entity.Order) (graphql.ID, error) {
	return resolve.ID(o.ID), nil
}

type OrderItemResolver struct{ client *velox.Client }

var _ orderitemgql.Resolver = (*OrderItemResolver)(nil)

func NewOrderItemResolver(client *velox.Client) *OrderItemResolver {
	return &OrderItemResolver{client: client}
}

func (r *OrderItemResolver) OrderItems(ctx context.Context) ([]*entity.OrderItem, error) {
	return resolve.List(r.client.OrderItem.Query().WithOrder().WithProduct().All(ctx))
}

func (r *OrderItemResolver) CreateOrderItem(ctx context.Context, args orderitemgql.CreateOrderItemArgs) (*entity.OrderItem, error) {
	it, err := r.client.OrderItem.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.OrderItem.Get(ctx, it.ID)
}

func (r *OrderItemResolver) OrderItemID(_ context.Context, it *entity.OrderItem) (graphql.ID, error) {
	return resolve.ID(it.ID), nil
}
