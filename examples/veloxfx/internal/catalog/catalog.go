// Package catalog implements the category and product groups.
//
// A domain package holds several entity groups, the way a gRPC server
// package can implement several services: one Resolver type per group, and
// one Module registering all of them.
package catalog

import (
	"context"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
	categorygql "github.com/syssam/graphql-go/examples/veloxfx/graph/category"
	productgql "github.com/syssam/graphql-go/examples/veloxfx/graph/product"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/orderby"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/resolve"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Module provides the domain's resolvers and registers their bindings.
var Module = fx.Module("catalog",
	fx.Provide(NewCategoryResolver, NewProductResolver),
	resolve.Bindings(func(r *CategoryResolver) graphql.SchemaOption { return categorygql.Bindings(r) }),
	resolve.Bindings(func(r *ProductResolver) graphql.SchemaOption { return productgql.Bindings(r) }),
)

type CategoryResolver struct{ client *velox.Client }

var _ categorygql.Resolver = (*CategoryResolver)(nil)

func NewCategoryResolver(client *velox.Client) *CategoryResolver {
	return &CategoryResolver{client: client}
}

func (r *CategoryResolver) Categories(ctx context.Context) ([]*entity.Category, error) {
	return resolve.List(r.client.Category.Query().WithProducts().All(ctx))
}

func (r *CategoryResolver) Category(ctx context.Context, args categorygql.CategoryArgs) (*entity.Category, error) {
	return resolve.Get(ctx, args.ID, r.client.Category.Get)
}

// CategoryProducts is Category.products, a connection. velox's edge method
// does the paging, filtering and ordering, and answers from the eager-loaded
// edge when no cursor, filter or order asks for more; the resolver exists only
// to convert the order argument, which gqlc could not bind to velox's type.
func (r *CategoryResolver) CategoryProducts(ctx context.Context, c *entity.Category, args categorygql.CategoryProductsArgs) (*entity.ProductConnection, error) {
	order, err := orderby.Product(args.OrderBy)
	if err != nil {
		return nil, err
	}
	return c.Products(ctx, args.After, args.First, args.Before, args.Last, order, args.Where)
}

func (r *CategoryResolver) CreateCategory(ctx context.Context, args categorygql.CreateCategoryArgs) (*entity.Category, error) {
	return r.client.Category.Create().SetInput(args.Input).Save(ctx)
}

func (r *CategoryResolver) UpdateCategory(ctx context.Context, args categorygql.UpdateCategoryArgs) (*entity.Category, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Category.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

// DeleteCategory is refused while products are in the category: the foreign
// key says so, and resolve.Delete reports it.
func (r *CategoryResolver) DeleteCategory(ctx context.Context, args categorygql.DeleteCategoryArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Category.DeleteOneID(id).Exec(ctx)
	})
}

func (r *CategoryResolver) CategoryID(_ context.Context, c *entity.Category) (graphql.ID, error) {
	return resolve.ID(c.ID), nil
}

type ProductResolver struct{ client *velox.Client }

var _ productgql.Resolver = (*ProductResolver)(nil)

func NewProductResolver(client *velox.Client) *ProductResolver {
	return &ProductResolver{client: client}
}

// Products is a connection: products(first: 10, after: $cursor,
// where: {priceCentsLT: 2000}, orderBy: {field: PRICE}).
func (r *ProductResolver) Products(ctx context.Context, args productgql.ProductsArgs) (*entity.ProductConnection, error) {
	order, err := orderby.Product(args.OrderBy)
	if err != nil {
		return nil, err
	}
	opts := []entity.ProductPaginateOption{entity.WithProductOrder(order)}
	if args.Where != nil {
		opts = append(opts, entity.WithProductFilter(args.Where.Filter))
	}
	q := r.client.Product.Query().WithCategory()
	return q.(entity.ProductPaginatable).Paginate(ctx, args.After, args.First, args.Before, args.Last, opts...)
}

func (r *ProductResolver) Product(ctx context.Context, args productgql.ProductArgs) (*entity.Product, error) {
	return resolve.Get(ctx, args.ID, r.client.Product.Get)
}

// CreateProduct reads the row back, for the reason the resolve package
// gives: Save leaves the category edge holding a stub.
func (r *ProductResolver) CreateProduct(ctx context.Context, args productgql.CreateProductArgs) (*entity.Product, error) {
	p, err := r.client.Product.Create().SetInput(args.Input).Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.client.Product.Get(ctx, p.ID)
}

func (r *ProductResolver) UpdateProduct(ctx context.Context, args productgql.UpdateProductArgs) (*entity.Product, error) {
	id, err := resolve.ParseID(args.ID)
	if err != nil {
		return nil, err
	}
	return r.client.Product.UpdateOneID(id).SetInput(args.Input).Save(ctx)
}

// DeleteProduct is declared in sdl/product.graphql, not by velox, which
// generates no delete mutation.
func (r *ProductResolver) DeleteProduct(ctx context.Context, args productgql.DeleteProductArgs) (graphql.ID, error) {
	return resolve.Delete(ctx, args.ID, func(ctx context.Context, id int) error {
		return r.client.Product.DeleteOneID(id).Exec(ctx)
	})
}

func (r *ProductResolver) ProductID(_ context.Context, p *entity.Product) (graphql.ID, error) {
	return resolve.ID(p.ID), nil
}
