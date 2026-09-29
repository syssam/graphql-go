// Package category is the category service: what may be done with a Category,
// whoever calls it; see package service for what that leaves to the caller.
package category

import (
	"context"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	categoryclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/category"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// List loads what the query selects beneath it and nothing else.
func (s *Service) List(ctx context.Context) ([]*entity.Category, error) {
	q, err := s.client.Category.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

// Get is the Category with this id, or nil if there is none.
func (s *Service) Get(ctx context.Context, id int) (*entity.Category, error) {
	v, err := s.client.Category.Get(ctx, id)
	return v, velox.MaskNotFound(err)
}

func (s *Service) Create(ctx context.Context, in categoryclient.CreateCategoryInput) (*entity.Category, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Category.Create().SetInput(in).Save(ctx)
}

func (s *Service) Update(ctx context.Context, id int, in categoryclient.UpdateCategoryInput) (*entity.Category, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Category.UpdateOneID(id).SetInput(in).Save(ctx)
}

// Delete is refused by the foreign key while products are in the category;
// the error presenter reports it as FAILED_PRECONDITION.
func (s *Service) Delete(ctx context.Context, id int) error {
	if err := viewer.RequireStaff(ctx); err != nil {
		return err
	}
	return s.client.Category.DeleteOneID(id).Exec(ctx)
}
