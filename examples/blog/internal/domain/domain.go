// Package domain holds the blog's business entities. It imports neither
// graphql-go nor the generated model package, so a change to the SDL cannot
// change the type the repository stores.
package domain

import "time"

type Role string

const (
	RoleAdmin Role = "ADMIN"
	RoleUser  Role = "USER"
)

type User struct {
	ID        string
	Name      string
	Email     string
	Role      Role
	CreatedAt time.Time
}

// Post carries AuthorID, which the generated model.Post does not: Post.author
// is a resolver field in the SDL but a plain column here.
type Post struct {
	ID          string
	Title       string
	Body        string
	AuthorID    string
	Tags        []string
	PublishedAt *time.Time
}

// Optional is a three-valued field: absent, present-and-nil ("clear it"), or
// present with a value. It exists so that PATCH semantics can cross into the
// repository without dragging graphql.Omittable in with them.
type Optional[T any] struct {
	Present bool
	Value   *T
}

func Absent[T any]() Optional[T] { return Optional[T]{} }

func Present[T any](v *T) Optional[T] { return Optional[T]{Present: true, Value: v} }

// PostUpdate is a partial update. An absent field is left alone; a present
// field holding nil clears the value.
type PostUpdate struct {
	Title Optional[string]
	Body  Optional[string]
}
