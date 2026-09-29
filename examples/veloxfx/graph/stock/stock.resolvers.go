package stock

import (
	"context"

	stocksvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/stock"
	entity "github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Handler implements the stock group's Resolver; each method hands its
// field to the stock service.
type Handler struct{ svc *stocksvc.Service }

func NewHandler(svc *stocksvc.Service) *Handler { return &Handler{svc: svc} }

var _ Resolver = (*Handler)(nil)

// CreateStock resolves Mutation.createStock.
func (r *Handler) CreateStock(ctx context.Context, args CreateStockArgs) (*entity.Stock, error) {
	return r.svc.Create(ctx, args.Input)
}

// AdjustStock resolves Mutation.adjustStock.
//
// Adds delta to the count, which may be negative but may not take it below zero.
func (r *Handler) AdjustStock(ctx context.Context, args AdjustStockArgs) (*entity.Stock, error) {
	return r.svc.Adjust(ctx, args.ID, args.Delta)
}

// DeleteStock resolves Mutation.deleteStock.
//
// Deletes the Stock and returns its id.
func (r *Handler) DeleteStock(ctx context.Context, args DeleteStockArgs) (int, error) {
	return args.ID, r.svc.Delete(ctx, args.ID)
}

// Stocks resolves Query.stocks.
func (r *Handler) Stocks(ctx context.Context) ([]*entity.Stock, error) {
	return r.svc.List(ctx)
}

// Stock resolves Query.stock.
//
// The Stock with this id, or null if there is none.
func (r *Handler) Stock(ctx context.Context, args StockArgs) (*entity.Stock, error) {
	return r.svc.Get(ctx, args.ID)
}
