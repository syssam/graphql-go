// Package inventory implements the warehouse and stock groups, and owns the
// one rule about stock every other domain relies on: a count never goes
// below zero. Take and Return are how sales moves it.
package inventory

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	stockgql "github.com/syssam/graphql-go/examples/veloxfx/graph/stock"
	warehousegql "github.com/syssam/graphql-go/examples/veloxfx/graph/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/orderby"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/product"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/warehouse"
)

// Module provides the domain's resolvers and registers their bindings.
var Module = fx.Module("inventory",
	fx.Provide(NewWarehouseResolver, NewStockResolver),
	resolve.Bindings(func(r *WarehouseResolver) graphql.SchemaOption { return warehousegql.Bindings(r) }),
	resolve.Bindings(func(r *StockResolver) graphql.SchemaOption { return stockgql.Bindings(r) }),
)

// ErrShort is Take's refusal: the warehouse holds fewer than were asked for.
var ErrShort = errors.New("not enough in stock")

// Take removes n of a product from a warehouse's stock, or refuses with
// ErrShort and changes nothing.
//
// It is one conditional update -- quantity = quantity - n WHERE quantity >= n
// -- rather than a read, a check and a write, so two takes racing for the
// last unit cannot both succeed: the second matches no row. The condition is
// the only guard. velox's NonNegative() on the column is checked when a value
// is set, not when one is added, so without it the count goes negative and
// the take succeeds.
func Take(ctx context.Context, tx *velox.Tx, warehouseID, productID, n int) error {
	taken, err := tx.Stock.Update().
		Where(
			stock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			stock.HasProductWith(product.IDField.EQ(productID)),
			stock.QuantityField.GTE(n),
		).
		AddQuantity(-n).
		Save(ctx)
	if err != nil {
		return err
	}
	if taken == 0 {
		return fmt.Errorf("%w for %d", ErrShort, n)
	}
	return nil
}

// Return puts n of a product back into a warehouse's stock, opening the row
// again if it was deleted since the take.
func Return(ctx context.Context, tx *velox.Tx, warehouseID, productID, n int) error {
	returned, err := tx.Stock.Update().
		Where(
			stock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			stock.HasProductWith(product.IDField.EQ(productID)),
		).
		AddQuantity(n).
		Save(ctx)
	if err != nil || returned > 0 {
		return err
	}
	_, err = tx.Stock.Create().SetWarehouseID(warehouseID).SetProductID(productID).SetQuantity(n).Save(ctx)
	return err
}

type WarehouseResolver struct{ client *velox.Client }

var _ warehousegql.Resolver = (*WarehouseResolver)(nil)

func NewWarehouseResolver(client *velox.Client) *WarehouseResolver {
	return &WarehouseResolver{client: client}
}

func (r *WarehouseResolver) Warehouses(ctx context.Context) ([]*entity.Warehouse, error) {
	return resolve.List(r.client.Warehouse.Query().
		WithStocks(func(q entity.StockQuerier) { q.WithProduct() }).
		All(ctx))
}

func (r *WarehouseResolver) Warehouse(ctx context.Context, args warehousegql.WarehouseArgs) (*entity.Warehouse, error) {
	return resolve.Get(ctx, args.ID, r.client.Warehouse.Get)
}

func (r *WarehouseResolver) CreateWarehouse(ctx context.Context, args warehousegql.CreateWarehouseArgs) (*entity.Warehouse, error) {
	return r.client.Warehouse.Create().SetInput(args.Input).Save(ctx)
}

func (r *WarehouseResolver) UpdateWarehouse(ctx context.Context, args warehousegql.UpdateWarehouseArgs) (*entity.Warehouse, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Warehouse.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

// DeleteWarehouse is refused while it holds stock rows, even empty ones, or
// orders were taken from it: deleting the stock first is the explicit way to
// retire one, and an order's warehouse is where a cancellation returns to.
func (r *WarehouseResolver) DeleteWarehouse(ctx context.Context, args warehousegql.DeleteWarehouseArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Warehouse.DeleteOneID(id).Exec(ctx)
	})
}

// WarehouseOrders is Warehouse.orders, the orders taken from it, a connection.
func (r *WarehouseResolver) WarehouseOrders(ctx context.Context, w *entity.Warehouse, args warehousegql.WarehouseOrdersArgs) (*entity.OrderConnection, error) {
	o, err := orderby.Order(args.OrderBy)
	if err != nil {
		return nil, err
	}
	return w.Orders(ctx, args.After, args.First, args.Before, args.Last, o, args.Where)
}

func (r *WarehouseResolver) WarehouseID(_ context.Context, w *entity.Warehouse) (graphql.ID, error) {
	return resolve.ID(w.ID), nil
}

type StockResolver struct{ client *velox.Client }

var _ stockgql.Resolver = (*StockResolver)(nil)

func NewStockResolver(client *velox.Client) *StockResolver {
	return &StockResolver{client: client}
}

func (r *StockResolver) Stocks(ctx context.Context) ([]*entity.Stock, error) {
	return resolve.List(r.client.Stock.Query().WithWarehouse().WithProduct().All(ctx))
}

func (r *StockResolver) Stock(ctx context.Context, args stockgql.StockArgs) (*entity.Stock, error) {
	return resolve.Get(ctx, args.ID, r.client.Stock.Get)
}

// CreateStock opens the row for a product in a warehouse. The unique index on
// (warehouse, product) refuses a second one.
func (r *StockResolver) CreateStock(ctx context.Context, args stockgql.CreateStockArgs) (*entity.Stock, error) {
	s, err := r.client.Stock.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		if velox.IsConstraintError(err) {
			return nil, errors.New("this warehouse already stocks this product; adjustStock changes its count")
		}
		return nil, err
	}
	return r.client.Stock.Get(ctx, s.ID)
}

// AdjustStock adds delta, which may be negative, with the same conditional
// update Take uses, so it composes with concurrent orders instead of
// overwriting what they took.
func (r *StockResolver) AdjustStock(ctx context.Context, args stockgql.AdjustStockArgs) (*entity.Stock, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	u := r.client.Stock.Update().Where(stock.IDField.EQ(id))
	if args.Delta < 0 {
		u = u.Where(stock.QuantityField.GTE(-args.Delta))
	}
	n, err := u.AddQuantity(args.Delta).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		s, err := r.client.Stock.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: holds %d, cannot remove %d", ErrShort, s.Quantity, -args.Delta)
	}
	return r.client.Stock.Get(ctx, id)
}

func (r *StockResolver) DeleteStock(ctx context.Context, args stockgql.DeleteStockArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Stock.DeleteOneID(id).Exec(ctx)
	})
}

func (r *StockResolver) StockID(_ context.Context, s *entity.Stock) (graphql.ID, error) {
	return resolve.ID(s.ID), nil
}
