package orderitem

import (
	"context"

	"github.com/syssam/graphql-go/examples/backoffice/internal/service"
	orderitemsvc "github.com/syssam/graphql-go/examples/backoffice/internal/service/orderitem"
	entity "github.com/syssam/graphql-go/examples/backoffice/velox/entity"
)

// Handler implements the orderitem group's Resolver; each method hands its
// field to the orderitem service.
type Handler struct{ svc *orderitemsvc.Service }

// NewHandler takes the services, as the generated graph.Wire passes them to
// every entity, and keeps the one this entity's fields go to.
func NewHandler(s service.Services) *Handler { return &Handler{svc: s.OrderItem} }

var _ Resolver = (*Handler)(nil)

// OrderItems resolves Query.orderItems.
func (r *Handler) OrderItems(ctx context.Context) ([]*entity.OrderItem, error) {
	return r.svc.List(ctx)
}

// OrderItem resolves Query.orderItem.
//
// The OrderItem with this id, or null if there is none.
func (r *Handler) OrderItem(ctx context.Context, args OrderItemArgs) (*entity.OrderItem, error) {
	return r.svc.Get(ctx, args.ID)
}
