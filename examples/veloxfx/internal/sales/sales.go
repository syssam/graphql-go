// Package sales implements the customer, order and orderitem groups.
//
// An order changes only through the operations in sdl/order.graphql, and
// each one is a conditional update on the state it moves from: two requests
// racing to ship and cancel the same order cannot both win, because the
// second one's WHERE status = ... matches no row.
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
	"github.com/syssam/graphql-go/examples/veloxfx/internal/inventory"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/orderby"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/orderitem"
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

// CustomerOrders is Customer.orders, a connection; see catalog's
// CategoryProducts.
func (r *CustomerResolver) CustomerOrders(ctx context.Context, c *entity.Customer, args customergql.CustomerOrdersArgs) (*entity.OrderConnection, error) {
	o, err := orderby.Order(args.OrderBy)
	if err != nil {
		return nil, err
	}
	return c.Orders(ctx, args.After, args.First, args.Before, args.Last, o, args.Where)
}

func (r *CustomerResolver) CreateCustomer(ctx context.Context, args customergql.CreateCustomerArgs) (*entity.Customer, error) {
	return r.client.Customer.Create().SetInput(args.Input).Save(ctx)
}

func (r *CustomerResolver) UpdateCustomer(ctx context.Context, args customergql.UpdateCustomerArgs) (*entity.Customer, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Customer.UpdateOneID(id).SetInput(args.Input).Save(ctx)
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
	o, err := orderby.Order(args.OrderBy)
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

// PlaceOrder is the only way an order comes to exist: the order, its items
// at the current price, and the stock they come out of, together or not at
// all. inventory.Take refuses a line the warehouse cannot cover, and the
// whole transaction rolls back with it.
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
		o, err := tx.Order.Create().SetCustomerID(customerID).SetWarehouseID(warehouseID).Save(ctx)
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
	if err := inventory.Take(ctx, tx, warehouseID, productID, line.Quantity); err != nil {
		return fmt.Errorf("%s: %w", p.Sku, err)
	}
	_, err = tx.OrderItem.Create().
		SetOrderID(orderID).
		SetProductID(productID).
		SetQuantity(line.Quantity).
		SetUnitPriceCents(p.PriceCents).
		Save(ctx)
	return err
}

func (r *OrderResolver) PayOrder(ctx context.Context, args ordergql.PayOrderArgs) (*entity.Order, error) {
	return r.move(ctx, args.ID, order.StatusPAID, order.StatusPENDING)
}

func (r *OrderResolver) ShipOrder(ctx context.Context, args ordergql.ShipOrderArgs) (*entity.Order, error) {
	return r.move(ctx, args.ID, order.StatusSHIPPED, order.StatusPAID)
}

// CancelOrder moves the order to CANCELLED and puts every item back into the
// warehouse it was taken from, in one transaction. A shipped order has left
// the warehouse and cannot be cancelled.
func (r *OrderResolver) CancelOrder(ctx context.Context, args ordergql.CancelOrderArgs) (*entity.Order, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	_, err = resolve.InTx(ctx, r.client, func(tx *velox.Tx) (struct{}, error) {
		if err := transition(ctx, tx.Client(), id, order.StatusCANCELLED, order.StatusPENDING, order.StatusPAID); err != nil {
			return struct{}{}, err
		}
		o, err := tx.Order.Query().
			Where(order.IDField.EQ(id)).
			WithWarehouse().
			WithItems(func(q entity.OrderItemQuerier) { q.WithProduct() }).
			Only(ctx)
		if err != nil {
			return struct{}{}, err
		}
		for _, it := range o.Edges.Items {
			if err := inventory.Return(ctx, tx, o.Edges.Warehouse.ID, it.Edges.Product.ID, it.Quantity); err != nil {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, id)
}

// DeleteOrder removes a cancelled order and its items. Any other order is a
// record of a sale, and its stock is either still taken or already shipped;
// cancelling first is what returns it.
func (r *OrderResolver) DeleteOrder(ctx context.Context, args ordergql.DeleteOrderArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		_, err := resolve.InTx(ctx, r.client, func(tx *velox.Tx) (struct{}, error) {
			if err := requireStatus(ctx, tx.Client(), id, order.StatusCANCELLED); err != nil {
				return struct{}{}, err
			}
			if _, err := tx.OrderItem.Delete().Where(orderitem.HasOrderWith(order.IDField.EQ(id))).Exec(ctx); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, tx.Order.DeleteOneID(id).Exec(ctx)
		})
		return err
	})
}

// move is one state transition, from any of from to to.
func (r *OrderResolver) move(ctx context.Context, gid graphql.ID, to order.Status, from ...order.Status) (*entity.Order, error) {
	id, err := resolve.ParseID(gid)
	if err != nil {
		return nil, err
	}
	if err := transition(ctx, r.client, id, to, from...); err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, id)
}

// transition sets the status only where it is still one of from. No row
// updated means the order is missing or in another state, and the error says
// which, so a client told "cannot ship" learns what the order is instead.
func transition(ctx context.Context, c *velox.Client, id int, to order.Status, from ...order.Status) error {
	n, err := c.Order.Update().
		Where(order.IDField.EQ(id), order.StatusField.In(from...)).
		SetStatus(to).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return requireStatus(ctx, c, id, from...)
	}
	return nil
}

func requireStatus(ctx context.Context, c *velox.Client, id int, want ...order.Status) error {
	o, err := c.Order.Get(ctx, id)
	if err != nil {
		return err
	}
	for _, s := range want {
		if o.Status == s {
			return nil
		}
	}
	return fmt.Errorf("order %d is %s, and this needs %v", id, o.Status, want)
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

func (r *OrderItemResolver) OrderItemID(_ context.Context, it *entity.OrderItem) (graphql.ID, error) {
	return resolve.ID(it.ID), nil
}
