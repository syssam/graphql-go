package customer

import (
	"context"

	customersvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/customer"
	entity "github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Handler implements the customer group's Resolver; each method hands its
// field to the customer service.
type Handler struct{ svc *customersvc.Service }

func NewHandler(svc *customersvc.Service) *Handler {
	return &Handler{svc: svc}
}

var _ Resolver = (*Handler)(nil)

// CreateCustomer resolves Mutation.createCustomer.
func (r *Handler) CreateCustomer(ctx context.Context, args CreateCustomerArgs) (*entity.Customer, error) {
	return r.svc.Create(ctx, args.Input)
}

// UpdateCustomer resolves Mutation.updateCustomer.
func (r *Handler) UpdateCustomer(ctx context.Context, args UpdateCustomerArgs) (*entity.Customer, error) {
	return r.svc.Update(ctx, args.ID, args.Input)
}

// DeleteCustomer resolves Mutation.deleteCustomer.
//
// Deletes the Customer and returns its id. Refused while other rows refer to it.
func (r *Handler) DeleteCustomer(ctx context.Context, args DeleteCustomerArgs) (int, error) {
	return args.ID, r.svc.Delete(ctx, args.ID)
}

// Customers resolves Query.customers.
func (r *Handler) Customers(ctx context.Context) ([]*entity.Customer, error) {
	return r.svc.List(ctx)
}

// Customer resolves Query.customer.
//
// The Customer with this id, or null if there is none.
func (r *Handler) Customer(ctx context.Context, args CustomerArgs) (*entity.Customer, error) {
	return r.svc.Get(ctx, args.ID)
}
