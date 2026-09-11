// Package schema is a hand-written binding of a small blog schema. It shows
// every binding constructor the runtime offers; generated code would produce
// the same calls.
package schema

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syssam/graphql-go"
)

// Role is the Go representation of the Role enum.
type Role int

const (
	RoleAdmin Role = iota
	RoleUser
)

// User is a blog author.
type User struct {
	ID        graphql.ID
	Name      string
	Email     string
	Role      Role
	CreatedAt time.Time
}

// Post is an article written by a User.
type Post struct {
	ID          graphql.ID
	Title       string
	Body        string
	AuthorID    graphql.ID
	Tags        []string
	PublishedAt *time.Time
}

// Store is an in-memory database seeded with a few records.
type Store struct {
	mu     sync.RWMutex
	users  map[graphql.ID]*User
	posts  []*Post
	nextID int
}

// NewStore returns a seeded store.
func NewStore() *Store {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	published := t0.Add(48 * time.Hour)
	return &Store{
		users: map[graphql.ID]*User{
			"1": {ID: "1", Name: "Alice", Email: "alice@example.com", Role: RoleAdmin, CreatedAt: t0},
			"2": {ID: "2", Name: "Bob", Email: "bob@example.com", Role: RoleUser, CreatedAt: t0.Add(time.Hour)},
		},
		posts: []*Post{
			{ID: "10", Title: "Hello", Body: "First post", AuthorID: "1", Tags: []string{"intro"}, PublishedAt: &published},
			{ID: "11", Title: "Go generics", Body: "Type parameters in practice", AuthorID: "1", Tags: []string{"go", "generics"}},
			{ID: "12", Title: "Draft", Body: "Work in progress", AuthorID: "2", Tags: []string{}},
		},
		nextID: 13,
	}
}

// User returns the user with the given id, or nil.
func (s *Store) User(id graphql.ID) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[id]
}

// Users lists all users ordered by id.
func (s *Store) Users() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b *User) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// Post returns the post with the given id, or nil.
func (s *Store) Post(id graphql.ID) *Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findPost(id)
}

func (s *Store) findPost(id graphql.ID) *Post {
	for _, p := range s.posts {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Posts lists posts matching the optional author and tag filters.
func (s *Store) Posts(authorID *graphql.ID, tag *string, limit int) []*Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Post, 0, len(s.posts))
	for _, p := range s.posts {
		if authorID != nil && p.AuthorID != *authorID {
			continue
		}
		if tag != nil && !slices.Contains(p.Tags, *tag) {
			continue
		}
		out = append(out, p)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Search returns users and posts whose name or title contains term.
func (s *Store) Search(term string) []any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	term = strings.ToLower(term)
	out := []any{}
	for _, u := range s.Users() {
		if strings.Contains(strings.ToLower(u.Name), term) {
			out = append(out, u)
		}
	}
	for _, p := range s.posts {
		if strings.Contains(strings.ToLower(p.Title), term) {
			out = append(out, p)
		}
	}
	return out
}

// CreatePost stores a new unpublished post.
func (s *Store) CreatePost(authorID graphql.ID, title, body string) (*Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[authorID] == nil {
		return nil, fmt.Errorf("author %q does not exist", authorID)
	}
	p := &Post{ID: graphql.ID(strconv.Itoa(s.nextID)), Title: title, Body: body, AuthorID: authorID, Tags: []string{}}
	s.nextID++
	s.posts = append(s.posts, p)
	return p, nil
}

// UpdatePost applies a partial update. Unset fields are left untouched; a
// field set to null clears the value.
func (s *Store) UpdatePost(id graphql.ID, title, body graphql.Omittable[*string]) (*Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.findPost(id)
	if p == nil {
		return nil, fmt.Errorf("post %q does not exist", id)
	}
	if v, ok := title.ValueOK(); ok {
		if v == nil {
			return nil, fmt.Errorf("title cannot be cleared")
		}
		p.Title = *v
	}
	if v, ok := body.ValueOK(); ok {
		if v == nil {
			p.Body = ""
		} else {
			p.Body = *v
		}
	}
	return p, nil
}
