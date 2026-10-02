// Package customer is the customer service: what may be done with a Customer,
// whoever calls it; see package service for what that leaves to the caller.
package customer

import (
	"context"

	"github.com/syssam/graphql-go/fed"
	"github.com/syssam/velox/contrib/graphqlgo"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	customerclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/customer"
	vcustomer "github.com/syssam/graphql-go/examples/veloxfx/velox/customer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// List loads what the query selects beneath it and nothing else.
func (s *Service) List(ctx context.Context) ([]*entity.Customer, error) {
	q, err := s.client.Customer.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

// Get is the Customer with this id, or nil if there is none.
func (s *Service) Get(ctx context.Context, id int) (*entity.Customer, error) {
	v, err := s.client.Customer.Get(ctx, id)
	return v, velox.MaskNotFound(err)
}

func (s *Service) Create(ctx context.Context, in customerclient.CreateCustomerInput) (*entity.Customer, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Customer.Create().SetInput(in).Save(ctx)
}

func (s *Service) Update(ctx context.Context, id int, in customerclient.UpdateCustomerInput) (*entity.Customer, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Customer.UpdateOneID(id).SetInput(in).Save(ctx)
}

// Delete is refused while the customer has orders: an order is a record of
// a sale, and deleting it to delete a customer is not this service's
// decision.
func (s *Service) Delete(ctx context.Context, id int) error {
	if err := viewer.RequireStaff(ctx); err != nil {
		return err
	}
	return s.client.Customer.DeleteOneID(id).Exec(ctx)
}

// Entity answers a federation router's references to customers: every one in
// a request is one query, collected for what the router selected.
func (s *Service) Entity() fed.Entity {
	return graphqlgo.Entities("Customer", graphqlgo.IntKey("id"),
		func(ctx context.Context, ids []int) ([]*entity.Customer, error) {
			q, err := s.client.Customer.Query().Where(vcustomer.IDField.In(ids...)).CollectFields(ctx, "Customer")
			if err != nil {
				return nil, err
			}
			return q.All(ctx)
		},
		func(c *entity.Customer) int { return c.ID })
}
