// Package orderitem is the order line service: what may be done with an
// OrderItem, whoever calls it; see package service for what that leaves to
// the caller.
//
// A line has no mutations: order.Place writes it, order.Delete removes it
// with its order.
package orderitem

import (
	"context"

	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// List loads what the query selects beneath it and nothing else.
func (s *Service) List(ctx context.Context) ([]*entity.OrderItem, error) {
	q, err := s.client.OrderItem.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

// Get is the OrderItem with this id, or nil if there is none.
func (s *Service) Get(ctx context.Context, id int) (*entity.OrderItem, error) {
	v, err := s.client.OrderItem.Get(ctx, id)
	return v, velox.MaskNotFound(err)
}
