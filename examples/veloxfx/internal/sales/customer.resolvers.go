package sales

import (
	"context"

	customergql "github.com/syssam/graphql-go/examples/veloxfx/graph/customer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// CustomerResolver implements the customer group's Resolver.
type CustomerResolver struct{ client *velox.Client }

var _ customergql.Resolver = (*CustomerResolver)(nil)

func NewCustomerResolver(client *velox.Client) *CustomerResolver {
	return &CustomerResolver{client: client}
}

func (r *CustomerResolver) Customers(ctx context.Context) ([]*entity.Customer, error) {
	return r.client.Customer.Query().WithOrders().All(ctx)
}

func (r *CustomerResolver) Customer(ctx context.Context, args customergql.CustomerArgs) (*entity.Customer, error) {
	c, err := r.client.Customer.Get(ctx, args.ID)
	return c, velox.MaskNotFound(err)
}

func (r *CustomerResolver) CreateCustomer(ctx context.Context, args customergql.CreateCustomerArgs) (*entity.Customer, error) {
	return r.client.Customer.Create().SetInput(args.Input).Save(ctx)
}

func (r *CustomerResolver) UpdateCustomer(ctx context.Context, args customergql.UpdateCustomerArgs) (*entity.Customer, error) {
	return r.client.Customer.UpdateOneID(args.ID).SetInput(args.Input).Save(ctx)
}

// DeleteCustomer is refused while the customer has orders: an order is a
// record of a sale, and deleting it to delete a customer is not this
// mutation's decision.
func (r *CustomerResolver) DeleteCustomer(ctx context.Context, args customergql.DeleteCustomerArgs) (int, error) {
	return args.ID, r.client.Customer.DeleteOneID(args.ID).Exec(ctx)
}
