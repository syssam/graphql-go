package schema

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/basic/graph"
	"github.com/syssam/graphql-go/examples/basic/graph/model"
	"github.com/syssam/graphql-go/loader"
)

type resolver struct {
	store   *Store
	authors *loader.Loader[graphql.ID, *model.User]
}

// NewSchema binds generated graph types to the store. Time, @upper and the
// author DataLoader stay hand-written.
func NewSchema(store *Store) (*graphql.Schema, error) {
	authors := loader.New(func(_ context.Context, ids []graphql.ID) (map[graphql.ID]*model.User, error) {
		return store.UsersByIDs(ids), nil
	})
	return graph.NewSchema(&resolver{store: store, authors: authors},
		graphql.Scalar("Time", marshalTime, unmarshalTime),
		graphql.Directive("upper", func(next graphql.FieldFunc) graphql.FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				if s, ok := v.(string); ok {
					return strings.ToUpper(s), err
				}
				return v, err
			}
		}),
	)
}

func (r *resolver) Node(_ context.Context, a graph.NodeArgs) (any, error) {
	if u := r.store.User(a.ID); u != nil {
		return u, nil
	}
	if p := r.store.Post(a.ID); p != nil {
		return p, nil
	}
	return nil, nil
}

func (r *resolver) User(_ context.Context, a graph.UserArgs) (*model.User, error) {
	return r.store.User(a.ID), nil
}

func (r *resolver) Users(context.Context) ([]*model.User, error) {
	return r.store.Users(), nil
}

func (r *resolver) Posts(_ context.Context, a graph.PostsArgs) ([]*model.Post, error) {
	if a.Filter == nil {
		return r.store.Posts(nil, nil, 0), nil
	}
	return r.store.Posts(a.Filter.AuthorID.Or(nil), a.Filter.Tag.Or(nil), 0), nil
}

func (r *resolver) Search(_ context.Context, a graph.SearchArgs) ([]any, error) {
	return r.store.Search(a.Term), nil
}

func (r *resolver) CreatePost(_ context.Context, a graph.CreatePostArgs) (*model.Post, error) {
	return r.store.CreatePost(a.AuthorID, a.Title, a.Body)
}

func (r *resolver) UpdatePost(_ context.Context, a graph.UpdatePostArgs) (*model.Post, error) {
	return r.store.UpdatePost(a.ID, a.Input.Title, a.Input.Body)
}

func (r *resolver) UserPosts(_ context.Context, u *model.User, a graph.UserPostsArgs) ([]*model.Post, error) {
	limit := 0
	if a.First != nil {
		limit = *a.First
	}
	return r.store.Posts(&u.ID, nil, limit), nil
}

func (r *resolver) PostAuthor(ctx context.Context, p *model.Post) (*model.User, error) {
	authorID := r.store.authorID(p.ID)
	u, err := r.authors.Load(ctx, authorID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("author %q of post %q is missing", authorID, p.ID)
	}
	return u, nil
}

func marshalTime(w *graphql.Writer, t time.Time) error {
	w.String(t.UTC().Format(time.RFC3339Nano))
	return nil
}

func unmarshalTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("Time must be an RFC 3339 string, got %T", v)
	}
	return time.Parse(time.RFC3339Nano, s)
}
