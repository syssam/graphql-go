package orderitem

import (
	"context"

	orderitemsvc "github.com/syssam/graphql-go/examples/backoffice/internal/service/orderitem"
	entity "github.com/syssam/graphql-go/examples/backoffice/velox/entity"
)

// Handler implements the orderitem group's Resolver; each method hands its
// field to the orderitem service.
type Handler struct{ svc *orderitemsvc.Service }

// NewHandler takes this entity's service and no other: the generated
// graph.Wire passes it services.OrderItem, so this package imports one service
// and an edit to another does not recompile it.
func NewHandler(svc *orderitemsvc.Service) *Handler { return &Handler{svc: svc} }

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
