package catalog

import (
	"context"

	productgql "github.com/syssam/graphql-go/examples/veloxfx/graph/product"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// ProductResolver implements the product group's Resolver.
type ProductResolver struct{ client *velox.Client }

var _ productgql.Resolver = (*ProductResolver)(nil)

func NewProductResolver(client *velox.Client) *ProductResolver {
	return &ProductResolver{client: client}
}

// Products is a Relay connection: products(first: 10, after: $cursor,
// where: {priceCentsLT: 2000}, orderBy: {field: PRICE}). The arguments are
// velox's own types, so they go straight to Paginate.
func (r *ProductResolver) Products(ctx context.Context, args productgql.ProductsArgs) (*entity.ProductConnection, error) {
	opts := []entity.ProductPaginateOption{entity.WithProductOrder(args.OrderBy)}
	if args.Where != nil {
		opts = append(opts, entity.WithProductFilter(args.Where.Filter))
	}
	// Paginate loads what the page's nodes select, category included.
	q := r.client.Product.Query()
	return q.Paginate(ctx, args.After, args.First, args.Before, args.Last, opts...)
}

func (r *ProductResolver) Product(ctx context.Context, args productgql.ProductArgs) (*entity.Product, error) {
	p, err := r.client.Product.Get(ctx, args.ID)
	return p, velox.MaskNotFound(err)
}

// CreateProduct returns the row Save created. Its category edge is left
// unloaded, so product { category { name } } queries it.
func (r *ProductResolver) CreateProduct(ctx context.Context, args productgql.CreateProductArgs) (*entity.Product, error) {
	return r.client.Product.Create().SetInput(args.Input).Save(ctx)
}

func (r *ProductResolver) UpdateProduct(ctx context.Context, args productgql.UpdateProductArgs) (*entity.Product, error) {
	return r.client.Product.UpdateOneID(args.ID).SetInput(args.Input).Save(ctx)
}

// DeleteProduct is declared in sdl/product.graphql: velox generates no
// delete. A product that is stocked or ordered is refused by the foreign key.
func (r *ProductResolver) DeleteProduct(ctx context.Context, args productgql.DeleteProductArgs) (int, error) {
	return args.ID, r.client.Product.DeleteOneID(args.ID).Exec(ctx)
}
