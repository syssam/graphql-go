// Package sales implements the customer, order and orderitem groups.
package sales

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	customergql "github.com/syssam/graphql-go/examples/veloxfx/graph/customer"
	ordermodel "github.com/syssam/graphql-go/examples/veloxfx/graph/model/order"
	ordergql "github.com/syssam/graphql-go/examples/veloxfx/graph/order"
	orderitemgql "github.com/syssam/graphql-go/examples/veloxfx/graph/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/product"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/warehouse"
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

func (r *CustomerResolver) Customer(ctx context.Context, args customergql.CustomerArgs) (*entity.Customer, error) {
	return resolve.Get(ctx, args.ID, r.client.Customer.Get)
}

// CustomerOrders is Customer.orders, a connection; see CategoryProducts.
func (r *CustomerResolver) CustomerOrders(ctx context.Context, c *entity.Customer, args customergql.CustomerOrdersArgs) (*entity.OrderConnection, error) {
	o, err := OrderOrder(args.OrderBy)
	if err != nil {
		return nil, err
	}
	return c.Orders(ctx, args.After, args.First, args.Before, args.Last, o, args.Where)
}

func (r *CustomerResolver) CreateCustomer(ctx context.Context, args customergql.CreateCustomerArgs) (*entity.Customer, error) {
	return r.client.Customer.Create().SetInput(args.Input).Save(ctx)
}

// DeleteCustomer is refused while the customer has orders: an order is a
// record of a sale, and deleting it to delete a customer is not this
// mutation's decision.
func (r *CustomerResolver) DeleteCustomer(ctx context.Context, args customergql.DeleteCustomerArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Customer.DeleteOneID(id).Exec(ctx)
	})
}

func (r *CustomerResolver) CustomerID(_ context.Context, c *entity.Customer) (graphql.ID, error) {
	return resolve.ID(c.ID), nil
}

type OrderResolver struct{ client *velox.Client }

var _ ordergql.Resolver = (*OrderResolver)(nil)

func NewOrderResolver(client *velox.Client) *OrderResolver {
	return &OrderResolver{client: client}
}

// Orders is a connection that eager-loads two levels, one of them crossing
// into the catalog domain: orders { edges { node { items { product } } } }
// is a fixed number of queries, not one per item.
func (r *OrderResolver) Orders(ctx context.Context, args ordergql.OrdersArgs) (*entity.OrderConnection, error) {
	o, err := OrderOrder(args.OrderBy)
	if err != nil {
		return nil, err
	}
	opts := []entity.OrderPaginateOption{entity.WithOrderOrder(o)}
	if args.Where != nil {
		opts = append(opts, entity.WithOrderFilter(args.Where.Filter))
	}
	q := r.client.Order.Query().
		WithCustomer().
		WithItems(func(q entity.OrderItemQuerier) { q.WithProduct() })
	return q.(entity.OrderPaginatable).Paginate(ctx, args.After, args.First, args.Before, args.Last, opts...)
}

func (r *OrderResolver) Order(ctx context.Context, args ordergql.OrderArgs) (*entity.Order, error) {
	return resolve.Get(ctx, args.ID, r.client.Order.Get)
}

func (r *OrderResolver) CreateOrder(ctx context.Context, args ordergql.CreateOrderArgs) (*entity.Order, error) {
	o, err := r.client.Order.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, o.ID)
}

// PlaceOrder is the mutation a shop actually takes: the order, its items at
// the current price, and the stock they come out of, together or not at all.
//
// Stock is taken with a conditional update -- quantity = quantity - n WHERE
// quantity >= n -- rather than read, checked and written, so two orders
// racing for the last unit cannot both succeed: the second update matches no
// row, and its whole transaction rolls back.
func (r *OrderResolver) PlaceOrder(ctx context.Context, args ordergql.PlaceOrderArgs) (*entity.Order, error) {
	in := args.Input
	if len(in.Items) == 0 {
		return nil, errors.New("an order needs at least one item")
	}
	customerID, err := resolve.ParseID(in.CustomerID)
	if err != nil {
		return nil, err
	}
	warehouseID, err := resolve.ParseID(in.WarehouseID)
	if err != nil {
		return nil, err
	}
	id, err := resolve.InTx(ctx, r.client, func(tx *velox.Tx) (int, error) {
		o, err := tx.Order.Create().SetCustomerID(customerID).Save(ctx)
		if err != nil {
			return 0, err
		}
		for i, line := range in.Items {
			if err := placeLine(ctx, tx, o.ID, warehouseID, line); err != nil {
				return 0, fmt.Errorf("items[%d]: %w", i, err)
			}
		}
		return o.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, id)
}

func placeLine(ctx context.Context, tx *velox.Tx, orderID, warehouseID int, line ordermodel.PlaceOrderItemInput) error {
	if line.Quantity <= 0 {
		return fmt.Errorf("quantity %d is not positive", line.Quantity)
	}
	productID, err := resolve.ParseID(line.ProductID)
	if err != nil {
		return err
	}
	p, err := tx.Product.Get(ctx, productID)
	if err != nil {
		return fmt.Errorf("product %s: %w", line.ProductID, err)
	}
	taken, err := tx.Stock.Update().
		Where(
			stock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			stock.HasProductWith(product.IDField.EQ(productID)),
			stock.QuantityField.GTE(line.Quantity),
		).
		AddQuantity(-line.Quantity).
		Save(ctx)
	if err != nil {
		return err
	}
	if taken == 0 {
		return fmt.Errorf("not enough %s in stock for %d", p.Sku, line.Quantity)
	}
	_, err = tx.OrderItem.Create().
		SetOrderID(orderID).
		SetProductID(productID).
		SetQuantity(line.Quantity).
		SetUnitPriceCents(p.PriceCents).
		Save(ctx)
	return err
}

func (r *OrderResolver) UpdateOrder(ctx context.Context, args ordergql.UpdateOrderArgs) (*entity.Order, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Order.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

// DeleteOrder deletes the order's items with it, in one transaction: items
// are part of the order, where a customer's orders are not part of the
// customer (see DeleteCustomer).
func (r *OrderResolver) DeleteOrder(ctx context.Context, args ordergql.DeleteOrderArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		_, err := resolve.InTx(ctx, r.client, func(tx *velox.Tx) (struct{}, error) {
			if _, err := tx.OrderItem.Delete().Where(orderitem.HasOrderWith(order.IDField.EQ(id))).Exec(ctx); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, tx.Order.DeleteOneID(id).Exec(ctx)
		})
		return err
	})
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

func (r *OrderItemResolver) OrderItem(ctx context.Context, args orderitemgql.OrderItemArgs) (*entity.OrderItem, error) {
	return resolve.Get(ctx, args.ID, r.client.OrderItem.Get)
}

func (r *OrderItemResolver) CreateOrderItem(ctx context.Context, args orderitemgql.CreateOrderItemArgs) (*entity.OrderItem, error) {
	it, err := r.client.OrderItem.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.OrderItem.Get(ctx, it.ID)
}

func (r *OrderItemResolver) DeleteOrderItem(ctx context.Context, args orderitemgql.DeleteOrderItemArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.OrderItem.DeleteOneID(id).Exec(ctx)
	})
}

func (r *OrderItemResolver) OrderItemID(_ context.Context, it *entity.OrderItem) (graphql.ID, error) {
	return resolve.ID(it.ID), nil
}

// OrderOrder converts an Order orderBy argument; see catalog.ProductOrder.
func OrderOrder(o *ordermodel.OrderOrder) (*entity.OrderOrder, error) {
	if o == nil {
		return nil, nil
	}
	f, err := resolve.OrderField[entity.OrderOrderField](string(o.Field))
	if err != nil {
		return nil, err
	}
	return &entity.OrderOrder{Direction: o.Direction, Field: f}, nil
}
