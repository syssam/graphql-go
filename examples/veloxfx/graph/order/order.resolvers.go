package order

import (
	"context"

	ordersvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/order"
	entity "github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Handler implements the order group's Resolver; each method hands its
// field to the order service.
type Handler struct{ svc *ordersvc.Service }

func NewHandler(svc *ordersvc.Service) *Handler { return &Handler{svc: svc} }

var _ Resolver = (*Handler)(nil)

// PlaceOrder resolves Mutation.placeOrder.
//
// Creates a PENDING order and its items at each product's current price, and
// takes the quantities out of the warehouse's stock -- all or nothing. A line
// the warehouse cannot cover fails the whole order and changes nothing.
func (r *Handler) PlaceOrder(ctx context.Context, args PlaceOrderArgs) (*entity.Order, error) {
	in := ordersvc.PlaceInput{CustomerID: args.Input.CustomerID, WarehouseID: args.Input.WarehouseID}
	for _, it := range args.Input.Items {
		in.Lines = append(in.Lines, ordersvc.Line{ProductID: it.ProductID, Quantity: it.Quantity})
	}
	return r.svc.Place(ctx, in)
}

// PayOrder resolves Mutation.payOrder.
//
// PENDING to PAID.
func (r *Handler) PayOrder(ctx context.Context, args PayOrderArgs) (*entity.Order, error) {
	return r.svc.Pay(ctx, args.ID)
}

// ShipOrder resolves Mutation.shipOrder.
//
// PAID to SHIPPED.
func (r *Handler) ShipOrder(ctx context.Context, args ShipOrderArgs) (*entity.Order, error) {
	return r.svc.Ship(ctx, args.ID)
}

// CancelOrder resolves Mutation.cancelOrder.
//
// PENDING or PAID to CANCELLED, returning the items to the warehouse's stock.
func (r *Handler) CancelOrder(ctx context.Context, args CancelOrderArgs) (*entity.Order, error) {
	return r.svc.Cancel(ctx, args.ID)
}

// DeleteOrder resolves Mutation.deleteOrder.
//
// Deletes a CANCELLED order and its items. Any other order is a record of a sale.
func (r *Handler) DeleteOrder(ctx context.Context, args DeleteOrderArgs) (int, error) {
	return args.ID, r.svc.Delete(ctx, args.ID)
}

// OrderTotalCents resolves Order.totalCents.
//
// Sum of quantity times unit price over the items.
func (r *Handler) OrderTotalCents(ctx context.Context, obj *entity.Order) (int, error) {
	return r.svc.TotalCents(ctx, obj)
}

// Orders resolves Query.orders.
func (r *Handler) Orders(ctx context.Context, args OrdersArgs) (*entity.OrderConnection, error) {
	return r.svc.Page(ctx, ordersvc.PageArgs(args))
}

// Order resolves Query.order.
//
// The Order with this id, or null if there is none.
func (r *Handler) Order(ctx context.Context, args OrderArgs) (*entity.Order, error) {
	return r.svc.Get(ctx, args.ID)
}
