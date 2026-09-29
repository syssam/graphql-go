package product

import (
	"context"

	productsvc "github.com/syssam/graphql-go/examples/veloxfx/internal/service/product"
	entity "github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// Handler implements the product group's Resolver; each method hands its
// field to the product service.
type Handler struct{ svc *productsvc.Service }

func NewHandler(svc *productsvc.Service) *Handler { return &Handler{svc: svc} }

var _ Resolver = (*Handler)(nil)

// CreateProduct resolves Mutation.createProduct.
func (r *Handler) CreateProduct(ctx context.Context, args CreateProductArgs) (*entity.Product, error) {
	return r.svc.Create(ctx, args.Input)
}

// UpdateProduct resolves Mutation.updateProduct.
func (r *Handler) UpdateProduct(ctx context.Context, args UpdateProductArgs) (*entity.Product, error) {
	return r.svc.Update(ctx, args.ID, args.Input)
}

// DeleteProduct resolves Mutation.deleteProduct.
//
// Deletes the Product and returns its id. Refused while stock or an order refers to it.
func (r *Handler) DeleteProduct(ctx context.Context, args DeleteProductArgs) (int, error) {
	return args.ID, r.svc.Delete(ctx, args.ID)
}

// Products resolves Query.products.
func (r *Handler) Products(ctx context.Context, args ProductsArgs) (*entity.ProductConnection, error) {
	return r.svc.Page(ctx, productsvc.PageArgs(args))
}

// Product resolves Query.product.
//
// The Product with this id, or null if there is none.
func (r *Handler) Product(ctx context.Context, args ProductArgs) (*entity.Product, error) {
	return r.svc.Get(ctx, args.ID)
}
