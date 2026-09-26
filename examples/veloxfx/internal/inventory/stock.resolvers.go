package inventory

import (
	"context"

	stockgql "github.com/syssam/graphql-go/examples/veloxfx/graph/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/stock"
)

// StockResolver implements the stock group's Resolver.
type StockResolver struct{ client *velox.Client }

var _ stockgql.Resolver = (*StockResolver)(nil)

func NewStockResolver(client *velox.Client) *StockResolver {
	return &StockResolver{client: client}
}

// Stocks loads what the query selects beneath it and nothing else.
func (r *StockResolver) Stocks(ctx context.Context) ([]*entity.Stock, error) {
	q, err := r.client.Stock.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

func (r *StockResolver) Stock(ctx context.Context, args stockgql.StockArgs) (*entity.Stock, error) {
	s, err := r.client.Stock.Get(ctx, args.ID)
	return s, velox.MaskNotFound(err)
}

// CreateStock opens the row for a product in a warehouse. The unique index on
// (warehouse, product) refuses a second one, as CONFLICT; adjustStock changes
// the count of the first.
func (r *StockResolver) CreateStock(ctx context.Context, args stockgql.CreateStockArgs) (*entity.Stock, error) {
	s, err := r.client.Stock.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.Stock.Get(ctx, s.ID)
}

// AdjustStock adds delta, which may be negative, with the same conditional
// update Take uses, so it composes with concurrent orders instead of
// overwriting what they took.
func (r *StockResolver) AdjustStock(ctx context.Context, args stockgql.AdjustStockArgs) (*entity.Stock, error) {
	u := r.client.Stock.Update().Where(stock.IDField.EQ(args.ID))
	if args.Delta < 0 {
		u = u.Where(stock.QuantityField.GTE(-args.Delta))
	}
	n, err := u.AddQuantity(args.Delta).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		s, err := r.client.Stock.Get(ctx, args.ID)
		if err != nil {
			return nil, err
		}
		return nil, short("holds %d, cannot remove %d", s.Quantity, -args.Delta)
	}
	return r.client.Stock.Get(ctx, args.ID)
}

func (r *StockResolver) DeleteStock(ctx context.Context, args stockgql.DeleteStockArgs) (int, error) {
	return args.ID, r.client.Stock.DeleteOneID(args.ID).Exec(ctx)
}
