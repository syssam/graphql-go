package stock

import (
	"context"
	"errors"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/query"
	"github.com/syssam/velox/dialect/sql"
)

// HideLevels makes every stock read find nothing for a viewer without
// inventory:read. Product.stocks is withheld by the GraphQL policy before it
// is queried; this is every other way to a Stock -- the stocks and stock
// roots, Warehouse.stocks, an eager load -- and it is in the query, so a
// shopper's request asks the database for no stock rows at all. A list is
// empty and a lookup is null, as Product.stocks is: not a lie a shopper can
// act on.
//
// Take and Return are updates, which an interceptor does not see, so an
// order still moves stock for the customer who places it.
func HideLevels(client *velox.Client) {
	client.Stock.Intercept(velox.TraverseFunc(func(ctx context.Context, q velox.Query) error {
		if viewer.From(ctx).Scopes()[viewer.ScopeInventory] {
			return nil
		}
		sq, ok := q.(*query.StockQuery)
		if !ok {
			return errors.New("HideLevels: unexpected query type")
		}
		sq.Where(func(s *sql.Selector) { s.Where(sql.False()) })
		return nil
	}))
}
