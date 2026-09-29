// Package warehouse is the warehouse service: what may be done with a Warehouse,
// whoever calls it; see package service for what that leaves to the caller.
package warehouse

import (
	"context"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	warehouseclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/warehouse"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// List loads what the query selects beneath it and nothing else.
func (s *Service) List(ctx context.Context) ([]*entity.Warehouse, error) {
	q, err := s.client.Warehouse.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

// Get is the Warehouse with this id, or nil if there is none.
func (s *Service) Get(ctx context.Context, id int) (*entity.Warehouse, error) {
	v, err := s.client.Warehouse.Get(ctx, id)
	return v, velox.MaskNotFound(err)
}

func (s *Service) Create(ctx context.Context, in warehouseclient.CreateWarehouseInput) (*entity.Warehouse, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Warehouse.Create().SetInput(in).Save(ctx)
}

func (s *Service) Update(ctx context.Context, id int, in warehouseclient.UpdateWarehouseInput) (*entity.Warehouse, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Warehouse.UpdateOneID(id).SetInput(in).Save(ctx)
}

// Delete is refused while the warehouse holds stock rows, even empty ones,
// or orders were taken from it: an order's warehouse is where a cancellation
// returns stock to.
func (s *Service) Delete(ctx context.Context, id int) error {
	if err := viewer.RequireStaff(ctx); err != nil {
		return err
	}
	return s.client.Warehouse.DeleteOneID(id).Exec(ctx)
}
