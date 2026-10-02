// Package stock is the stock service, and owns the one rule about stock every
// other service relies on: a count never goes below zero. Take and Return are
// how an order moves it.
package stock

import (
	"context"
	"errors"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/apperr"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	stockclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/product"
	vstock "github.com/syssam/graphql-go/examples/veloxfx/velox/stock"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/warehouse"
)

// ErrShort is the stock rule's refusal: the warehouse holds fewer than were
// asked for. It reaches a client as FAILED_PRECONDITION.
var ErrShort = errors.New("not enough in stock")

func short(format string, args ...any) error {
	return apperr.Wrap(ErrShort, apperr.FailedPrecondition, format, args...)
}

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// List loads what the query selects beneath it and nothing else.
func (s *Service) List(ctx context.Context) ([]*entity.Stock, error) {
	q, err := s.client.Stock.Query().CollectFields(ctx)
	if err != nil {
		return nil, err
	}
	return q.All(ctx)
}

// Get is the Stock with this id, or nil if there is none.
func (s *Service) Get(ctx context.Context, id int) (*entity.Stock, error) {
	st, err := s.client.Stock.Get(ctx, id)
	return st, velox.MaskNotFound(err)
}

// Create opens the row for a product in a warehouse. The unique index on
// (warehouse, product) refuses a second one, as CONFLICT; Adjust changes the
// count of the first.
func (s *Service) Create(ctx context.Context, in stockclient.CreateStockInput) (*entity.Stock, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Stock.Create().SetInput(in).Save(ctx)
}

// Adjust adds delta, which may be negative, with the same conditional update
// Take uses, so it composes with concurrent orders instead of overwriting
// what they took.
func (s *Service) Adjust(ctx context.Context, id, delta int) (*entity.Stock, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	u := s.client.Stock.Update().Where(vstock.IDField.EQ(id))
	if delta < 0 {
		u = u.Where(vstock.QuantityField.GTE(-delta))
	}
	n, err := u.AddQuantity(delta).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		st, err := s.client.Stock.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		return nil, short("holds %d, cannot remove %d", st.Quantity, -delta)
	}
	return s.client.Stock.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id int) error {
	if err := viewer.RequireStaff(ctx); err != nil {
		return err
	}
	return s.client.Stock.DeleteOneID(id).Exec(ctx)
}

// Take removes n of a product from a warehouse's stock, or refuses with
// ErrShort and changes nothing.
//
// It is one conditional update -- quantity = quantity - n WHERE quantity >= n
// -- rather than a read, a check and a write, so two takes racing for the
// last unit cannot both succeed: the second matches no row. velox's
// NonNegative() on the column is checked when a value is set, not when one is
// added; the CHECK constraint FeatureCheckBounds puts on the column would
// still refuse a negative count, but as a constraint error, where the
// condition makes it the "not enough in stock" a client can act on.
func Take(ctx context.Context, tx *velox.Tx, warehouseID, productID, n int) error {
	taken, err := tx.Stock.Update().
		Where(
			vstock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			vstock.HasProductWith(product.IDField.EQ(productID)),
			vstock.QuantityField.GTE(n),
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
			vstock.HasWarehouseWith(warehouse.IDField.EQ(warehouseID)),
			vstock.HasProductWith(product.IDField.EQ(productID)),
		).
		AddQuantity(n).
		Save(ctx)
	if err != nil || returned > 0 {
		return err
	}
	_, err = tx.Stock.Create().SetWarehouseID(warehouseID).SetProductID(productID).SetQuantity(n).Save(ctx)
	return err
}
