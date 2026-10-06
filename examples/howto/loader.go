package howto

import (
	"context"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

const blogSDL = `
type Author { id: ID! name: String! }
type Post { id: ID! title: String! author: Author! }
type Query { posts: [Post!]! }
`

type Author struct{ ID, Name string }

// Post carries AuthorID and not an *Author, because that is what a row of a
// posts table holds. The author is a second lookup.
type Post struct{ ID, Title, AuthorID string }

// Authors is what the store can answer: one row by its key, or many rows by
// their keys in one round trip. A loader needs the second.
type Authors interface {
	ByID(ctx context.Context, id string) (*Author, error)
	ByIDs(ctx context.Context, ids []string) (map[string]*Author, error)
}

// NewNaiveBlogSchema is the N+1: Post.author asks the store once per post.
func NewNaiveBlogSchema(posts []*Post, authors Authors) (*graphql.Schema, error) {
	return newBlogSchema(posts,
		graphql.Resolve("author", func(ctx context.Context, p *Post) (*Author, error) {
			return authors.ByID(ctx, p.AuthorID)
		}),
	)
}

// NewBlogSchema is the same schema with Post.author behind a loader.
func NewBlogSchema(posts []*Post, authors Authors) (*graphql.Schema, error) {
	// Built once and captured by the resolver. A loader keeps each request's
	// cache and queue apart by itself, so one value serves every request;
	// built inside the resolver it would be a new one per post.
	authorByID := loader.New(authors.ByIDs)

	return newBlogSchema(posts,
		graphql.Resolve("author", func(ctx context.Context, p *Post) (*Author, error) {
			return authorByID.Load(ctx, p.AuthorID)
		}),
	)
}

// newBlogSchema takes Post.author as a parameter so the two schemas above
// differ in that one field and nothing else.
func newBlogSchema(posts []*Post, author graphql.FieldOption) (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(blogSDL),
		graphql.Object[Author]("Author",
			graphql.Field("id", func(a *Author) graphql.ID { return graphql.ID(a.ID) }),
			graphql.Field("name", func(a *Author) string { return a.Name }),
		),
		graphql.Object[Post]("Post",
			graphql.Field("id", func(p *Post) graphql.ID { return graphql.ID(p.ID) }),
			graphql.Field("title", func(p *Post) string { return p.Title }),
			author,
		),
		graphql.Query(
			graphql.Resolve("posts", func(context.Context, graphql.Root) ([]*Post, error) {
				return posts, nil
			}),
		),
	)
}
