package veloxfx

import (
	"context"

	"github.com/syssam/graphql-go/fed"
	"github.com/syssam/velox/contrib/graphqlgo"

	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/customer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/order"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/product"
)

// entities answers the router: every reference another subgraph holds to a
// product, customer or order arrives in one _entities call, and each type is
// one query for all of them -- collected for what the router selected, and
// through the same client as every other read, so the ownership filter on
// orders applies to a router's fetch exactly as to a client's.
func entities(client *velox.Client) []fed.Entity {
	return []fed.Entity{
		graphqlgo.Entities("Product", graphqlgo.IntKey("id"),
			func(ctx context.Context, ids []int) ([]*entity.Product, error) {
				q, err := client.Product.Query().Where(product.IDField.In(ids...)).CollectFields(ctx, "Product")
				if err != nil {
					return nil, err
				}
				return q.All(ctx)
			},
			func(p *entity.Product) int { return p.ID }),
		graphqlgo.Entities("Customer", graphqlgo.IntKey("id"),
			func(ctx context.Context, ids []int) ([]*entity.Customer, error) {
				q, err := client.Customer.Query().Where(customer.IDField.In(ids...)).CollectFields(ctx, "Customer")
				if err != nil {
					return nil, err
				}
				return q.All(ctx)
			},
			func(c *entity.Customer) int { return c.ID }),
		graphqlgo.Entities("Order", graphqlgo.IntKey("id"),
			func(ctx context.Context, ids []int) ([]*entity.Order, error) {
				q, err := client.Order.Query().Where(order.IDField.In(ids...)).CollectFields(ctx, "Order")
				if err != nil {
					return nil, err
				}
				return q.All(ctx)
			},
			func(o *entity.Order) int { return o.ID }),
	}
}
