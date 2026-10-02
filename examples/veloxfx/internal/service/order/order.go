// Package order is the order service. An order changes only through its
// operations here, and each one is a conditional update on the state it
// moves from: two requests racing to ship and cancel the same order cannot
// both win, because the second one's WHERE status = ... matches no row.
package order

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/syssam/graphql-go/fed"
	"github.com/syssam/velox/contrib/graphql/gqlrelay"
	"github.com/syssam/velox/contrib/graphqlgo"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/apperr"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/service/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/filter"
	vorder "github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/orderitem"
)

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// PageArgs is a Relay page request, in velox's own types and in the order
// the generated args hold them, so a caller converts rather than copies.
type PageArgs struct {
	After   *gqlrelay.Cursor
	First   *int
	Before  *gqlrelay.Cursor
	Last    *int
	OrderBy *entity.OrderOrder
	Where   *filter.OrderWhereInput
}

// Page is a connection whose page loads what its nodes select, across
// entities: orders { edges { node { items { product } } } } is a fixed number
// of queries, not one per item, and no COUNT unless totalCount is asked for.
// totalCents declares in schema/sales.go that it reads the items, so velox
// loads them whole when a node selects it.
func (s *Service) Page(ctx context.Context, p PageArgs) (*entity.OrderConnection, error) {
	opts := []entity.OrderPaginateOption{entity.WithOrderOrder(p.OrderBy)}
	if p.Where != nil {
		opts = append(opts, entity.WithOrderFilter(p.Where.Filter))
	}
	return s.client.Order.Query().Paginate(ctx, p.After, p.First, p.Before, p.Last, opts...)
}

// Get is the Order with this id, or nil if there is none or it is not the
// viewer's.
func (s *Service) Get(ctx context.Context, id int) (*entity.Order, error) {
	o, err := s.client.Order.Get(ctx, id)
	return o, velox.MaskNotFound(err)
}

// PlaceInput is an order to place: every line comes out of one warehouse.
type PlaceInput struct {
	CustomerID  int
	WarehouseID int
	Lines       []Line
}

// Line is one product and how many of it.
type Line struct {
	ProductID int
	Quantity  int
}

// Place is the only way an order comes to exist: the order, its items at
// the current price, and the stock they come out of, together or not at all.
// stock.Take refuses a line the warehouse cannot cover, and the whole
// transaction rolls back with it.
//
// A customer places orders as themselves; a CustomerID naming anyone else is
// refused rather than corrected, so a caller with the wrong id learns so.
// Staff place orders for any customer, and nobody signed in places none.
func (s *Service) Place(ctx context.Context, in PlaceInput) (*entity.Order, error) {
	switch v := viewer.From(ctx); {
	case v.Staff:
	case v.Anonymous():
		return nil, errSignIn
	case in.CustomerID != v.CustomerID:
		return nil, apperr.New(apperr.Forbidden, "customer %d cannot place an order for customer %d", v.CustomerID, in.CustomerID)
	}
	if len(in.Lines) == 0 {
		return nil, apperr.New(apperr.BadUserInput, "an order needs at least one item")
	}
	var id int
	err := withTx(ctx, s.client, func(tx *velox.Tx) error {
		o, err := tx.Order.Create().SetCustomerID(in.CustomerID).SetWarehouseID(in.WarehouseID).Save(ctx)
		if err != nil {
			return err
		}
		for i, line := range in.Lines {
			if err := placeLine(ctx, tx, o.ID, in.WarehouseID, line); err != nil {
				return apperr.Prefixed(fmt.Sprintf("items[%d]", i), err)
			}
		}
		id = o.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.client.Order.Get(ctx, id)
}

func placeLine(ctx context.Context, tx *velox.Tx, orderID, warehouseID int, line Line) error {
	if line.Quantity <= 0 {
		return apperr.New(apperr.BadUserInput, "quantity %d is not positive", line.Quantity)
	}
	p, err := tx.Product.Get(ctx, line.ProductID)
	if err != nil {
		return err
	}
	if err := stock.Take(ctx, tx, warehouseID, p.ID, line.Quantity); err != nil {
		return apperr.Prefixed(p.Sku, err)
	}
	_, err = tx.OrderItem.Create().
		SetOrderID(orderID).
		SetProductID(p.ID).
		SetQuantity(line.Quantity).
		SetUnitPriceCents(p.PriceCents).
		Save(ctx)
	return err
}

// Pay moves an order from PENDING to PAID.
func (s *Service) Pay(ctx context.Context, id int) (*entity.Order, error) {
	return s.move(ctx, id, vorder.StatusPAID, vorder.StatusPENDING)
}

// Ship moves an order from PAID to SHIPPED.
func (s *Service) Ship(ctx context.Context, id int) (*entity.Order, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.move(ctx, id, vorder.StatusSHIPPED, vorder.StatusPAID)
}

// Cancel moves the order to CANCELLED and puts every item back into the
// warehouse it was taken from, in one transaction. A shipped order has left
// the warehouse and cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, id int) (*entity.Order, error) {
	err := withTx(ctx, s.client, func(tx *velox.Tx) error {
		if err := transition(ctx, tx.Client(), id, vorder.StatusCANCELLED, vorder.StatusPENDING, vorder.StatusPAID); err != nil {
			return err
		}
		o, err := tx.Order.Query().
			Where(vorder.IDField.EQ(id)).
			WithWarehouse().
			WithItems(func(q entity.OrderItemQuerier) { q.WithProduct() }).
			Only(ctx)
		if err != nil {
			return err
		}
		for _, it := range o.Edges.Items {
			if err := stock.Return(ctx, tx, o.Edges.Warehouse.ID, it.Edges.Product.ID, it.Quantity); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.client.Order.Get(ctx, id)
}

// Delete removes a cancelled order and its items. Any other order is a
// record of a sale, and its stock is either still taken or already shipped;
// cancelling first is what returns it.
func (s *Service) Delete(ctx context.Context, id int) error {
	if err := viewer.RequireStaff(ctx); err != nil {
		return err
	}
	return withTx(ctx, s.client, func(tx *velox.Tx) error {
		if err := requireStatus(ctx, tx.Client(), id, vorder.StatusCANCELLED); err != nil {
			return err
		}
		if _, err := tx.OrderItem.Delete().Where(orderitem.HasOrderWith(vorder.IDField.EQ(id))).Exec(ctx); err != nil {
			return err
		}
		return tx.Order.DeleteOneID(id).Exec(ctx)
	})
}

// TotalCents sums quantity times unit price over the order's items.
// schema/sales.go declares Order.totalCents with Loads("items"): wherever
// orders are collected -- a page, a customer's orders -- the items are
// already loaded whole, and a single order queries them once.
func (s *Service) TotalCents(ctx context.Context, o *entity.Order) (int, error) {
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

// Entity answers a federation router's references to orders through the same
// client as every other read, so OwnOrders applies to a router's fetch
// exactly as to a client's.
func (s *Service) Entity() fed.Entity {
	return graphqlgo.Entities("Order", graphqlgo.IntKey("id"),
		func(ctx context.Context, ids []int) ([]*entity.Order, error) {
			q, err := s.client.Order.Query().Where(vorder.IDField.In(ids...)).CollectFields(ctx, "Order")
			if err != nil {
				return nil, err
			}
			return q.All(ctx)
		},
		func(o *entity.Order) int { return o.ID })
}

// move is one state transition, from any of from to to.
func (s *Service) move(ctx context.Context, id int, to vorder.Status, from ...vorder.Status) (*entity.Order, error) {
	if err := transition(ctx, s.client, id, to, from...); err != nil {
		return nil, err
	}
	return s.client.Order.Get(ctx, id)
}

// transition sets the status only where it is still one of from, and only
// on an order the viewer owns: OwnOrders narrows reads, and an UPDATE is not
// a read. No row updated means the order is missing, someone else's, or in
// another state, and the error says which it can: requireStatus reads the
// order through the same filter, so someone else's is NOT_FOUND, exactly as
// a read of it would be.
func transition(ctx context.Context, c *velox.Client, id int, to vorder.Status, from ...vorder.Status) error {
	mine, err := owned(ctx)
	if err != nil {
		return err
	}
	n, err := c.Order.Update().
		Where(append(mine, vorder.IDField.EQ(id), vorder.StatusField.In(from...))...).
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

func requireStatus(ctx context.Context, c *velox.Client, id int, want ...vorder.Status) error {
	o, err := c.Order.Get(ctx, id)
	if err != nil {
		return err
	}
	if slices.Contains(want, o.Status) {
		return nil
	}
	return apperr.New(apperr.FailedPrecondition, "order %d is %s, and this needs %v", id, o.Status, want)
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
