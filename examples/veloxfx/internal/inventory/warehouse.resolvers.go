package inventory

import (
	"context"

	warehousegql "github.com/syssam/graphql-go/examples/veloxfx/graph/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// WarehouseResolver implements the warehouse group's Resolver.
type WarehouseResolver struct{ client *velox.Client }

var _ warehousegql.Resolver = (*WarehouseResolver)(nil)

func NewWarehouseResolver(client *velox.Client) *WarehouseResolver {
	return &WarehouseResolver{client: client}
}

// Warehouses loads what the query selects beneath it, across domains:
// warehouses { stocks { product { name } } } is three queries in all.
func (r *WarehouseResolver) Warehouses(ctx context.Context) ([]*entity.Warehouse, error) {
	q, err := r.client.Warehouse.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

func (r *WarehouseResolver) Warehouse(ctx context.Context, args warehousegql.WarehouseArgs) (*entity.Warehouse, error) {
	w, err := r.client.Warehouse.Get(ctx, args.ID)
	return w, velox.MaskNotFound(err)
}

func (r *WarehouseResolver) CreateWarehouse(ctx context.Context, args warehousegql.CreateWarehouseArgs) (*entity.Warehouse, error) {
	return r.client.Warehouse.Create().SetInput(args.Input).Save(ctx)
}

func (r *WarehouseResolver) UpdateWarehouse(ctx context.Context, args warehousegql.UpdateWarehouseArgs) (*entity.Warehouse, error) {
	return r.client.Warehouse.UpdateOneID(args.ID).SetInput(args.Input).Save(ctx)
}

// DeleteWarehouse is refused while it holds stock rows, even empty ones, or
// orders were taken from it: an order's warehouse is where a cancellation
// returns stock to.
func (r *WarehouseResolver) DeleteWarehouse(ctx context.Context, args warehousegql.DeleteWarehouseArgs) (int, error) {
	return args.ID, r.client.Warehouse.DeleteOneID(args.ID).Exec(ctx)
}
