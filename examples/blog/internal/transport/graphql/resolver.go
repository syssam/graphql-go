// Package graphql adapts the domain to the generated GraphQL bindings. It is
// the only package that knows both domain types and generated model types.
package graphql

import (
	"context"
	"fmt"
	"slices"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/blog/graph"
	"github.com/syssam/graphql-go/examples/blog/graph/model"
	"github.com/syssam/graphql-go/examples/blog/internal/app"
	"github.com/syssam/graphql-go/examples/blog/internal/domain"
	"github.com/syssam/graphql-go/loader"
)

// Resolver implements graph.Resolver.
type Resolver struct {
	svc     *app.Service
	authors *loader.Loader[graphql.ID, *model.User]
}

var _ graph.Resolver = (*Resolver)(nil)

// New builds the resolver. The DataLoader lives here rather than in app
// because it exists to batch the N+1 that GraphQL's field-at-a-time resolution
// creates; nothing outside this adapter would ever want one.
func New(svc *app.Service) *Resolver {
	r := &Resolver{svc: svc}
	r.authors = loader.New(func(_ context.Context, ids []graphql.ID) (map[graphql.ID]*model.User, error) {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = string(id)
		}
		found := svc.UsersByIDs(keys)
		out := make(map[graphql.ID]*model.User, len(found))
		for k, u := range found {
			out[graphql.ID(k)] = toUser(u)
		}
		return out, nil
	})
	return r
}

func toUser(u *domain.User) *model.User {
	if u == nil {
		return nil
	}
	return &model.User{
		ID:        graphql.ID(u.ID),
		Name:      u.Name,
		Email:     u.Email,
		Role:      model.Role(u.Role),
		CreatedAt: u.CreatedAt,
	}
}

func toPost(p *domain.Post) *model.Post {
	if p == nil {
		return nil
	}
	return &model.Post{
		ID:          graphql.ID(p.ID),
		Title:       p.Title,
		Body:        p.Body,
		Tags:        p.Tags,
		PublishedAt: p.PublishedAt,
	}
}

func toPosts(ps []*domain.Post) []*model.Post {
	out := make([]*model.Post, len(ps))
	for i, p := range ps {
		out[i] = toPost(p)
	}
	return out
}

// optional converts the GraphQL layer's three-valued Omittable into the
// domain's own, so nothing below this package imports graphql.Omittable.
func optional(o graphql.Omittable[*string]) domain.Optional[string] {
	v, ok := o.ValueOK()
	if !ok {
		return domain.Absent[string]()
	}
	return domain.Present(v)
}

func (r *Resolver) Node(_ context.Context, a graph.NodeArgs) (model.Node, error) {
	if u := r.svc.User(string(a.ID)); u != nil {
		return toUser(u), nil
	}
	if p := r.svc.Post(string(a.ID)); p != nil {
		return toPost(p), nil
	}
	return nil, nil
}

func (r *Resolver) User(_ context.Context, a graph.UserArgs) (*model.User, error) {
	return toUser(r.svc.User(string(a.ID))), nil
}

func (r *Resolver) Users(context.Context) ([]*model.User, error) {
	users := r.svc.Users()
	out := make([]*model.User, len(users))
	for i, u := range users {
		out[i] = toUser(u)
	}
	return out, nil
}

func (r *Resolver) Posts(_ context.Context, a graph.PostsArgs) ([]*model.Post, error) {
	if a.Filter == nil {
		return toPosts(r.svc.Posts(nil, nil, 0)), nil
	}
	var authorID *string
	if v, ok := a.Filter.AuthorID.ValueOK(); ok && v != nil {
		s := string(*v)
		authorID = &s
	}
	return toPosts(r.svc.Posts(authorID, a.Filter.Tag.Or(nil), 0)), nil
}

func (r *Resolver) Search(_ context.Context, a graph.SearchArgs) ([]model.SearchResult, error) {
	users, posts := r.svc.Search(a.Term)
	out := make([]model.SearchResult, 0, len(users)+len(posts))
	for _, u := range users {
		out = append(out, toUser(u))
	}
	for _, p := range posts {
		out = append(out, toPost(p))
	}
	return out, nil
}

func (r *Resolver) CreatePost(_ context.Context, a graph.CreatePostArgs) (*model.Post, error) {
	p, err := r.svc.CreatePost(string(a.AuthorID), a.Title, a.Body, a.Tags)
	if err != nil {
		return nil, err
	}
	return toPost(p), nil
}

func (r *Resolver) UpdatePost(_ context.Context, a graph.UpdatePostArgs) (*model.Post, error) {
	p, err := r.svc.UpdatePost(string(a.ID), domain.PostUpdate{
		Title: optional(a.Input.Title),
		Body:  optional(a.Input.Body),
	})
	if err != nil {
		return nil, err
	}
	return toPost(p), nil
}

func (r *Resolver) UserPosts(_ context.Context, u *model.User, a graph.UserPostsArgs) ([]*model.Post, error) {
	limit := 0
	if a.First != nil {
		limit = *a.First
	}
	id := string(u.ID)
	return toPosts(r.svc.Posts(&id, nil, limit)), nil
}

// PostAuthor pays for the domain/model split. model.Post has no AuthorID,
// because Post.author is a resolver field in the SDL, so the author id has to
// be fetched back out of the store by post id before the loader can batch the
// user lookup. Binding Object[domain.Post] directly would hand it over free.
func (r *Resolver) PostAuthor(ctx context.Context, p *model.Post) (*model.User, error) {
	d := r.svc.Post(string(p.ID))
	if d == nil {
		return nil, fmt.Errorf("post %q does not exist", p.ID)
	}
	u, err := r.authors.Load(ctx, graphql.ID(d.AuthorID))
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("author %q of post %q is missing", d.AuthorID, p.ID)
	}
	return u, nil
}

// PostCreated streams posts as CreatePost stores them.
func (r *Resolver) PostCreated(ctx context.Context) (<-chan *model.Post, error) {
	return r.stream(ctx, func(*domain.Post) bool { return true }), nil
}

// PostCreatedWithTag is the filtered form. The filter runs in a goroutine
// between the source and the subscriber rather than inside the broker, so one
// client's predicate cannot slow down publishing to the others.
func (r *Resolver) PostCreatedWithTag(ctx context.Context, a graph.PostCreatedWithTagArgs) (<-chan *model.Post, error) {
	return r.stream(ctx, func(p *domain.Post) bool { return slices.Contains(p.Tags, a.Tag) }), nil
}

func (r *Resolver) stream(ctx context.Context, keep func(*domain.Post) bool) <-chan *model.Post {
	src := r.svc.PostsCreated(ctx)
	out := make(chan *model.Post)
	go func() {
		defer close(out)
		for p := range src {
			if !keep(p) {
				continue
			}
			select {
			case out <- toPost(p):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
