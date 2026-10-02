package category

import (
	"context"

	"github.com/syssam/graphql-go/examples/backoffice/internal/service"
	categorysvc "github.com/syssam/graphql-go/examples/backoffice/internal/service/category"
	entity "github.com/syssam/graphql-go/examples/backoffice/velox/entity"
)

// Handler implements the category group's Resolver; each method hands its
// field to the category service.
type Handler struct{ svc *categorysvc.Service }

// NewHandler takes the services, as the generated graph.Wire passes them to
// every entity, and keeps the one this entity's fields go to.
func NewHandler(s service.Services) *Handler { return &Handler{svc: s.Category} }

var _ Resolver = (*Handler)(nil)

// CreateCategory resolves Mutation.createCategory.
func (r *Handler) CreateCategory(ctx context.Context, args CreateCategoryArgs) (*entity.Category, error) {
	return r.svc.Create(ctx, args.Input)
}

// UpdateCategory resolves Mutation.updateCategory.
func (r *Handler) UpdateCategory(ctx context.Context, args UpdateCategoryArgs) (*entity.Category, error) {
	return r.svc.Update(ctx, args.ID, args.Input)
}

// DeleteCategory resolves Mutation.deleteCategory.
//
// Deletes the Category and returns its id. Refused while other rows refer to it.
func (r *Handler) DeleteCategory(ctx context.Context, args DeleteCategoryArgs) (int, error) {
	return args.ID, r.svc.Delete(ctx, args.ID)
}

// Categories resolves Query.categories.
func (r *Handler) Categories(ctx context.Context) ([]*entity.Category, error) {
	return r.svc.List(ctx)
}

// Category resolves Query.category.
//
// The Category with this id, or null if there is none.
func (r *Handler) Category(ctx context.Context, args CategoryArgs) (*entity.Category, error) {
	return r.svc.Get(ctx, args.ID)
}
