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

// OrderItems loads what the query selects beneath it and nothing else.
func (r *OrderItemResolver) OrderItems(ctx context.Context) ([]*entity.OrderItem, error) {
	q, err := r.client.OrderItem.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

func (r *OrderItemResolver) OrderItem(ctx context.Context, args orderitemgql.OrderItemArgs) (*entity.OrderItem, error) {
	it, err := r.client.OrderItem.Get(ctx, args.ID)
	return it, velox.MaskNotFound(err)
}
