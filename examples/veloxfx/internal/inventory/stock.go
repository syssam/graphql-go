package inventory

import (
	"context"
	"errors"
	"fmt"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/product"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/warehouse"
)

// ErrShort is the stock rule's refusal: the warehouse holds fewer than were
// asked for. It reaches a client as FAILED_PRECONDITION.
var ErrShort = errors.New("not enough in stock")

func short(format string, args ...any) error {
	return (&graphql.Error{Message: fmt.Sprintf(format, args...), Err: ErrShort}).
		WithExtension("code", "FAILED_PRECONDITION")
}

// Take removes n of a product from a warehouse's stock, or refuses with
// ErrShort and changes nothing.
//
// It is one conditional update -- quantity = quantity - n WHERE quantity >= n
// -- rather than a read, a check and a write, so two takes racing for the
// last unit cannot both succeed: the second matches no row. The condition is
// the only guard. velox's NonNegative() on the column is checked when a value
// is set, not when one is added, so without it the count goes negative and
// the take succeeds.
func Take(ctx context.Context, tx *velox.Tx, warehouseID, productID, n int) error {
	taken, err := tx.Stock.Update().
		Where(
			stock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			stock.HasProductWith(product.IDField.EQ(productID)),
			stock.QuantityField.GTE(n),
		).
		AddQuantity(-n).
		Save(ctx)
	if err != nil {
		return err
	}
	if taken == 0 {
		return short("not enough in stock for %d", n)
	}
	return nil
}

// Return puts n of a product back into a warehouse's stock, opening the row
// again if it was deleted since the take.
func Return(ctx context.Context, tx *velox.Tx, warehouseID, productID, n int) error {
	returned, err := tx.Stock.Update().
		Where(
			stock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			stock.HasProductWith(product.IDField.EQ(productID)),
		).
		AddQuantity(n).
		Save(ctx)
	if err != nil || returned > 0 {
		return err
	}
	_, err = tx.Stock.Create().SetWarehouseID(warehouseID).SetProductID(productID).SetQuantity(n).Save(ctx)
	return err
}
