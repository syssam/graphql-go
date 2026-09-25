package sales

import (
	"context"

	orderitemgql "github.com/syssam/graphql-go/examples/veloxfx/graph/orderitem"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// OrderItemResolver implements the orderitem group's Resolver. A line has no
// mutations: placeOrder writes it, deleteOrder removes it with its order.
type OrderItemResolver struct{ client *velox.Client }

var _ orderitemgql.Resolver = (*OrderItemResolver)(nil)

func NewOrderItemResolver(client *velox.Client) *OrderItemResolver {
	return &OrderItemResolver{client: client}
}

func (r *OrderItemResolver) OrderItems(ctx context.Context) ([]*entity.OrderItem, error) {
	return r.client.OrderItem.Query().WithOrder().WithProduct().All(ctx)
}

func (r *OrderItemResolver) OrderItem(ctx context.Context, args orderitemgql.OrderItemArgs) (*entity.OrderItem, error) {
	it, err := r.client.OrderItem.Get(ctx, args.ID)
	return it, velox.MaskNotFound(err)
}
