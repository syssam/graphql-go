// Package inventory implements the warehouse and stock groups.
package inventory

import (
	"context"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	stockgql "github.com/syssam/graphql-go/examples/veloxfx/graph/stock"
	warehousegql "github.com/syssam/graphql-go/examples/veloxfx/graph/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Module provides the domain's resolvers and registers their bindings.
var Module = fx.Module("inventory",
	fx.Provide(NewWarehouseResolver, NewStockResolver),
	resolve.Bindings(func(r *WarehouseResolver) graphql.SchemaOption { return warehousegql.Bindings(r) }),
	resolve.Bindings(func(r *StockResolver) graphql.SchemaOption { return stockgql.Bindings(r) }),
)

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

func (r *WarehouseResolver) CreateWarehouse(ctx context.Context, args warehousegql.CreateWarehouseArgs) (*entity.Warehouse, error) {
	return r.client.Warehouse.Create().SetInput(args.Input).Save(ctx)
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

func (r *StockResolver) CreateStock(ctx context.Context, args stockgql.CreateStockArgs) (*entity.Stock, error) {
	s, err := r.client.Stock.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.Stock.Get(ctx, s.ID)
}

func (r *StockResolver) UpdateStock(ctx context.Context, args stockgql.UpdateStockArgs) (*entity.Stock, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Stock.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

func (r *StockResolver) StockID(_ context.Context, s *entity.Stock) (graphql.ID, error) {
	return resolve.ID(s.ID), nil
}

func (r *WarehouseResolver) Warehouse(ctx context.Context, args warehousegql.WarehouseArgs) (*entity.Warehouse, error) {
	return resolve.Get(ctx, args.ID, r.client.Warehouse.Get)
}

// DeleteWarehouse is refused while it holds stock rows, even empty ones:
// zeroing and deleting the stock first is the explicit way to retire one.
func (r *WarehouseResolver) DeleteWarehouse(ctx context.Context, args warehousegql.DeleteWarehouseArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Warehouse.DeleteOneID(id).Exec(ctx)
	})
}

func (r *StockResolver) Stock(ctx context.Context, args stockgql.StockArgs) (*entity.Stock, error) {
	return resolve.Get(ctx, args.ID, r.client.Stock.Get)
}

func (r *StockResolver) DeleteStock(ctx context.Context, args stockgql.DeleteStockArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Stock.DeleteOneID(id).Exec(ctx)
	})
}
