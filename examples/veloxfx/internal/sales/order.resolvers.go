package sales

import (
	"context"
	"errors"
	"fmt"
	"slices"

	graphql "github.com/syssam/graphql-go"
	ordermodel "github.com/syssam/graphql-go/examples/veloxfx/graph/model/order"
	ordergql "github.com/syssam/graphql-go/examples/veloxfx/graph/order"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/inventory"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/orderitem"
	"github.com/syssam/velox/contrib/graphqlgo"
)

// OrderResolver implements the order group's Resolver.
type OrderResolver struct{ client *velox.Client }

var _ ordergql.Resolver = (*OrderResolver)(nil)

func NewOrderResolver(client *velox.Client) *OrderResolver {
	return &OrderResolver{client: client}
}

// Orders is a connection whose page loads what its nodes select, across
// domains: orders { edges { node { items { product } } } } is a fixed number
// of queries, not one per item, and no COUNT unless totalCount is asked for.
//
// totalCents is the one thing Paginate cannot see: it is sdl/order.graphql's,
// and it reads every item's price whatever the client selected beneath
// items. Loading the items here, when a node asks for it, is what tells
// velox to leave them whole.
func (r *OrderResolver) Orders(ctx context.Context, args ordergql.OrdersArgs) (*entity.OrderConnection, error) {
	opts := []entity.OrderPaginateOption{entity.WithOrderOrder(args.OrderBy)}
	if args.Where != nil {
		opts = append(opts, entity.WithOrderFilter(args.Where.Filter))
	}
	q := r.client.Order.Query()
	if graphqlgo.NodeSelects(ctx, "totalCents") {
		q = q.WithItems()
	}
	return q.(entity.OrderPaginatable).Paginate(ctx, args.After, args.First, args.Before, args.Last, opts...)
}

func (r *OrderResolver) Order(ctx context.Context, args ordergql.OrderArgs) (*entity.Order, error) {
	o, err := r.client.Order.Get(ctx, args.ID)
	return o, velox.MaskNotFound(err)
}

// PlaceOrder is the only way an order comes to exist: the order, its items at
// the current price, and the stock they come out of, together or not at all.
// inventory.Take refuses a line the warehouse cannot cover, and the whole
// transaction rolls back with it.
func (r *OrderResolver) PlaceOrder(ctx context.Context, args ordergql.PlaceOrderArgs) (*entity.Order, error) {
	in := args.Input
	if len(in.Items) == 0 {
		return nil, badInput("an order needs at least one item")
	}
	var id int
	err := withTx(ctx, r.client, func(tx *velox.Tx) error {
		o, err := tx.Order.Create().SetCustomerID(in.CustomerID).SetWarehouseID(in.WarehouseID).Save(ctx)
		if err != nil {
			return err
		}
		for i, line := range in.Items {
			if err := placeLine(ctx, tx, o.ID, in.WarehouseID, line); err != nil {
				return prefixed(fmt.Sprintf("items[%d]", i), err)
			}
		}
		id = o.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, id)
}

func placeLine(ctx context.Context, tx *velox.Tx, orderID, warehouseID int, line ordermodel.PlaceOrderItemInput) error {
	if line.Quantity <= 0 {
		return badInput("quantity %d is not positive", line.Quantity)
	}
	p, err := tx.Product.Get(ctx, line.ProductID)
	if err != nil {
		return err
	}
	if err := inventory.Take(ctx, tx, warehouseID, p.ID, line.Quantity); err != nil {
		return prefixed(p.Sku, err)
	}
	_, err = tx.OrderItem.Create().
		SetOrderID(orderID).
		SetProductID(p.ID).
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
	err := withTx(ctx, r.client, func(tx *velox.Tx) error {
		if err := transition(ctx, tx.Client(), args.ID, order.StatusCANCELLED, order.StatusPENDING, order.StatusPAID); err != nil {
			return err
		}
		o, err := tx.Order.Query().
			Where(order.IDField.EQ(args.ID)).
			WithWarehouse().
			WithItems(func(q entity.OrderItemQuerier) { q.WithProduct() }).
			Only(ctx)
		if err != nil {
			return err
		}
		for _, it := range o.Edges.Items {
			if err := inventory.Return(ctx, tx, o.Edges.Warehouse.ID, it.Edges.Product.ID, it.Quantity); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.client.Order.Get(ctx, args.ID)
}

// DeleteOrder removes a cancelled order and its items. Any other order is a
// record of a sale, and its stock is either still taken or already shipped;
// cancelling first is what returns it.
func (r *OrderResolver) DeleteOrder(ctx context.Context, args ordergql.DeleteOrderArgs) (int, error) {
	return args.ID, withTx(ctx, r.client, func(tx *velox.Tx) error {
		if err := requireStatus(ctx, tx.Client(), args.ID, order.StatusCANCELLED); err != nil {
			return err
		}
		if _, err := tx.OrderItem.Delete().Where(orderitem.HasOrderWith(order.IDField.EQ(args.ID))).Exec(ctx); err != nil {
			return err
		}
		return tx.Order.DeleteOneID(args.ID).Exec(ctx)
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

// move is one state transition, from any of from to to.
func (r *OrderResolver) move(ctx context.Context, id int, to order.Status, from ...order.Status) (*entity.Order, error) {
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
	if slices.Contains(want, o.Status) {
		return nil
	}
	return (&graphql.Error{Message: fmt.Sprintf("order %d is %s, and this needs %v", id, o.Status, want)}).
		WithExtension("code", "FAILED_PRECONDITION")
}

// withTx runs fn in one transaction: committed if fn returns no error, rolled
// back if it does. fn must use tx's clients, or its writes escape it.
func withTx(ctx context.Context, c *velox.Client, fn func(tx *velox.Tx) error) error {
	tx, err := c.Tx(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func badInput(format string, args ...any) error {
	return (&graphql.Error{Message: fmt.Sprintf(format, args...)}).WithExtension("code", "BAD_USER_INPUT")
}

// prefixed puts where into a coded error's message and keeps its code; any
// other error is left for the presenter.
func prefixed(where string, err error) error {
	var gerr *graphql.Error
	if errors.As(err, &gerr) {
		out := *gerr
		out.Message = where + ": " + gerr.Message
		return &out
	}
	return err
}
