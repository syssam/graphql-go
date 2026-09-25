package catalog

import (
	"context"

	categorygql "github.com/syssam/graphql-go/examples/veloxfx/graph/category"
	"github.com/syssam/graphql-go/examples/veloxfx/velox"
	"github.com/syssam/graphql-go/examples/veloxfx/velox/entity"
)

// CategoryResolver implements the category group's Resolver.
type CategoryResolver struct{ client *velox.Client }

var _ categorygql.Resolver = (*CategoryResolver)(nil)

func NewCategoryResolver(client *velox.Client) *CategoryResolver {
	return &CategoryResolver{client: client}
}

// Categories eager-loads products, so Category.products -- velox's edge
// method, bound as it is -- answers a first page from memory.
func (r *CategoryResolver) Categories(ctx context.Context) ([]*entity.Category, error) {
	return r.client.Category.Query().WithProducts().All(ctx)
}

func (r *CategoryResolver) Category(ctx context.Context, args categorygql.CategoryArgs) (*entity.Category, error) {
	c, err := r.client.Category.Get(ctx, args.ID)
	return c, velox.MaskNotFound(err)
}

func (r *CategoryResolver) CreateCategory(ctx context.Context, args categorygql.CreateCategoryArgs) (*entity.Category, error) {
	return r.client.Category.Create().SetInput(args.Input).Save(ctx)
}

func (r *CategoryResolver) UpdateCategory(ctx context.Context, args categorygql.UpdateCategoryArgs) (*entity.Category, error) {
	return r.client.Category.UpdateOneID(args.ID).SetInput(args.Input).Save(ctx)
}

// DeleteCategory is refused by the foreign key while products are in the
// category; the error presenter reports it as FAILED_PRECONDITION.
func (r *CategoryResolver) DeleteCategory(ctx context.Context, args categorygql.DeleteCategoryArgs) (int, error) {
	return args.ID, r.client.Category.DeleteOneID(args.ID).Exec(ctx)
}
