package schema

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/syssam/graphql-go"
)

//go:embed schema.graphql
var sdl string

// Argument and input structs. Nullable SDL positions use pointers so that
// null stays distinguishable from the zero value.
type (
	idArgs        struct{ ID graphql.ID }
	userPostsArgs struct{ First *int }
	postsArgs     struct{ Filter *PostFilter }
	searchArgs    struct{ Term string }

	createPostArgs struct {
		AuthorID graphql.ID
		Title    string
		Body     string
	}
	updatePostArgs struct {
		ID    graphql.ID
		Input UpdatePostInput
	}

	// PostFilter mirrors the PostFilter input object.
	PostFilter struct {
		AuthorID *graphql.ID
		Tag      *string
	}

	// UpdatePostInput mirrors the UpdatePostInput input object. Omittable
	// records whether each field was present in the request.
	UpdatePostInput struct {
		Title graphql.Omittable[*string]
		Body  graphql.Omittable[*string]
	}
)

// NewSchema binds the SDL to the store.
func NewSchema(store *Store) (*graphql.Schema, error) {
	return graphql.NewSchema(graphql.SDL(sdl),
		graphql.Scalar("Time", marshalTime, unmarshalTime),
		graphql.Enum("Role", map[Role]string{RoleAdmin: "ADMIN", RoleUser: "USER"}),

		graphql.Directive("upper", func(next graphql.FieldFunc, _ struct{}) graphql.FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				if s, ok := v.(string); ok {
					return strings.ToUpper(s), err
				}
				return v, err
			}
		}),

		graphql.Input[PostFilter]("PostFilter",
			graphql.InputField("authorId", func(f *PostFilter, v *graphql.ID) { f.AuthorID = v }),
			graphql.InputField("tag", func(f *PostFilter, v *string) { f.Tag = v }),
		),
		graphql.Input[UpdatePostInput]("UpdatePostInput",
			graphql.OmittableField("title", func(i *UpdatePostInput, v graphql.Omittable[*string]) { i.Title = v }),
			graphql.OmittableField("body", func(i *UpdatePostInput, v graphql.Omittable[*string]) { i.Body = v }),
		),

		graphql.Args[idArgs](graphql.InputField("id", func(a *idArgs, v graphql.ID) { a.ID = v })),
		graphql.Args[userPostsArgs](graphql.InputField("first", func(a *userPostsArgs, v *int) { a.First = v })),
		graphql.Args[postsArgs](graphql.InputField("filter", func(a *postsArgs, v *PostFilter) { a.Filter = v })),
		graphql.Args[searchArgs](graphql.InputField("term", func(a *searchArgs, v string) { a.Term = v })),
		graphql.Args[createPostArgs](
			graphql.InputField("authorId", func(a *createPostArgs, v graphql.ID) { a.AuthorID = v }),
			graphql.InputField("title", func(a *createPostArgs, v string) { a.Title = v }),
			graphql.InputField("body", func(a *createPostArgs, v string) { a.Body = v }),
		),
		graphql.Args[updatePostArgs](
			graphql.InputField("id", func(a *updatePostArgs, v graphql.ID) { a.ID = v }),
			graphql.InputField("input", func(a *updatePostArgs, v UpdatePostInput) { a.Input = v }),
		),

		// Abstract types resolve their concrete type from the Go value, so
		// these bindings only document intent.
		graphql.Interface[any]("Node"),
		graphql.Union[any]("SearchResult"),

		graphql.Object[User]("User",
			graphql.Field("id", func(u *User) graphql.ID { return u.ID }),
			graphql.Field("name", func(u *User) string { return u.Name }),
			graphql.Field("email", func(u *User) string { return u.Email }),
			graphql.Field("role", func(u *User) Role { return u.Role }),
			graphql.Field("createdAt", func(u *User) time.Time { return u.CreatedAt }),
			graphql.ResolveArgs("posts", func(_ context.Context, u *User, a userPostsArgs) ([]*Post, error) {
				limit := 0
				if a.First != nil {
					limit = *a.First
				}
				return store.Posts(&u.ID, nil, limit), nil
			}),
		),
		graphql.Object[Post]("Post",
			graphql.Field("id", func(p *Post) graphql.ID { return p.ID }),
			graphql.Field("title", func(p *Post) string { return p.Title }),
			graphql.Field("body", func(p *Post) string { return p.Body }),
			graphql.Field("tags", func(p *Post) []string { return p.Tags }),
			graphql.Field("publishedAt", func(p *Post) *time.Time { return p.PublishedAt }),
			graphql.Resolve("author", func(_ context.Context, p *Post) (*User, error) {
				u := store.User(p.AuthorID)
				if u == nil {
					return nil, fmt.Errorf("author %q of post %q is missing", p.AuthorID, p.ID)
				}
				return u, nil
			}),
		),

		graphql.Object[graphql.Root]("Query",
			graphql.ResolveArgs("node", func(_ context.Context, _ graphql.Root, a idArgs) (any, error) {
				if u := store.User(a.ID); u != nil {
					return u, nil
				}
				if p := store.Post(a.ID); p != nil {
					return p, nil
				}
				return nil, nil
			}),
			graphql.ResolveArgs("user", func(_ context.Context, _ graphql.Root, a idArgs) (*User, error) {
				return store.User(a.ID), nil
			}),
			graphql.Resolve("users", func(context.Context, graphql.Root) ([]*User, error) {
				return store.Users(), nil
			}),
			graphql.ResolveArgs("posts", func(_ context.Context, _ graphql.Root, a postsArgs) ([]*Post, error) {
				if a.Filter == nil {
					return store.Posts(nil, nil, 0), nil
				}
				return store.Posts(a.Filter.AuthorID, a.Filter.Tag, 0), nil
			}),
			graphql.ResolveArgs("search", func(_ context.Context, _ graphql.Root, a searchArgs) ([]any, error) {
				return store.Search(a.Term), nil
			}),
		),
		graphql.Object[graphql.Root]("Mutation",
			graphql.ResolveArgs("createPost", func(_ context.Context, _ graphql.Root, a createPostArgs) (*Post, error) {
				return store.CreatePost(a.AuthorID, a.Title, a.Body)
			}),
			graphql.ResolveArgs("updatePost", func(_ context.Context, _ graphql.Root, a updatePostArgs) (*Post, error) {
				return store.UpdatePost(a.ID, a.Input.Title, a.Input.Body)
			}),
		),
	)
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
