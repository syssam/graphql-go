package warehouse

import (
	"context"

	warehousesvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/warehouse"
	entity "github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Handler implements the warehouse group's Resolver; each method hands its
// field to the warehouse service.
type Handler struct{ svc *warehousesvc.Service }

func NewHandler(svc *warehousesvc.Service) *Handler {
	return &Handler{svc: svc}
}

var _ Resolver = (*Handler)(nil)

// CreateWarehouse resolves Mutation.createWarehouse.
func (r *Handler) CreateWarehouse(ctx context.Context, args CreateWarehouseArgs) (*entity.Warehouse, error) {
	return r.svc.Create(ctx, args.Input)
}

// UpdateWarehouse resolves Mutation.updateWarehouse.
func (r *Handler) UpdateWarehouse(ctx context.Context, args UpdateWarehouseArgs) (*entity.Warehouse, error) {
	return r.svc.Update(ctx, args.ID, args.Input)
}

// DeleteWarehouse resolves Mutation.deleteWarehouse.
//
// Deletes the Warehouse and returns its id. Refused while other rows refer to it.
func (r *Handler) DeleteWarehouse(ctx context.Context, args DeleteWarehouseArgs) (int, error) {
	return args.ID, r.svc.Delete(ctx, args.ID)
}

// Warehouses resolves Query.warehouses.
func (r *Handler) Warehouses(ctx context.Context) ([]*entity.Warehouse, error) {
	return r.svc.List(ctx)
}

// Warehouse resolves Query.warehouse.
//
// The Warehouse with this id, or null if there is none.
func (r *Handler) Warehouse(ctx context.Context, args WarehouseArgs) (*entity.Warehouse, error) {
	return r.svc.Get(ctx, args.ID)
}
