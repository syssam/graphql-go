// Package product is the product service: what may be done with a Product,
// whoever calls it; see package service for what that leaves to the caller.
package product

import (
	"context"

	"github.com/syssam/graphql-go/fed"
	"github.com/syssam/velox/contrib/graphql/gqlrelay"
	"github.com/syssam/velox/contrib/graphqlgo"

	"github.com/syssam/graphql-go/examples/veloxfx/internal/viewer"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	productclient "github.com/syssam/graphql-go/examples/veloxfx/velox/client/product"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/filter"
	vproduct "github.com/syssam/graphql-go/examples/veloxfx/velox/product"
)

type Service struct{ client *velox.Client }

func New(client *velox.Client) *Service { return &Service{client: client} }

// PageArgs is a Relay page request: products(first: 10, after: $cursor,
// where: {priceCentsLT: 2000}, orderBy: {field: PRICE}). Its fields are
// velox's own types, in the order the generated args hold them, so a caller
// converts rather than copies.
type PageArgs struct {
	After   *gqlrelay.Cursor
	First   *int
	Before  *gqlrelay.Cursor
	Last    *int
	OrderBy *entity.ProductOrder
	Where   *filter.ProductWhereInput
}

// Page loads what the page's nodes select, category included.
func (s *Service) Page(ctx context.Context, p PageArgs) (*entity.ProductConnection, error) {
	opts := []entity.ProductPaginateOption{entity.WithProductOrder(p.OrderBy)}
	if p.Where != nil {
		opts = append(opts, entity.WithProductFilter(p.Where.Filter))
	}
	return s.client.Product.Query().Paginate(ctx, p.After, p.First, p.Before, p.Last, opts...)
}

// Get is the Product with this id, or nil if there is none.
func (s *Service) Get(ctx context.Context, id int) (*entity.Product, error) {
	p, err := s.client.Product.Get(ctx, id)
	return p, velox.MaskNotFound(err)
}

// Create returns the row Save created. Its category edge is left unloaded,
// so product { category { name } } queries it.
func (s *Service) Create(ctx context.Context, in productclient.CreateProductInput) (*entity.Product, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Product.Create().SetInput(in).Save(ctx)
}

func (s *Service) Update(ctx context.Context, id int, in productclient.UpdateProductInput) (*entity.Product, error) {
	if err := viewer.RequireStaff(ctx); err != nil {
		return nil, err
	}
	return s.client.Product.UpdateOneID(id).SetInput(in).Save(ctx)
}

// Delete is refused by the foreign key while the product is stocked or
// ordered.
func (s *Service) Delete(ctx context.Context, id int) error {
	if err := viewer.RequireStaff(ctx); err != nil {
		return err
	}
	return s.client.Product.DeleteOneID(id).Exec(ctx)
}

// Entity answers a federation router's references to products: every one in
// a request is one query, collected for what the router selected.
func (s *Service) Entity() fed.Entity {
	return graphqlgo.Entities("Product", graphqlgo.IntKey("id"),
		func(ctx context.Context, ids []int) ([]*entity.Product, error) {
			q, err := s.client.Product.Query().Where(vproduct.IDField.In(ids...)).CollectFields(ctx, "Product")
			if err != nil {
				return nil, err
			}
			return q.All(ctx)
		},
		func(p *entity.Product) int { return p.ID })
}
