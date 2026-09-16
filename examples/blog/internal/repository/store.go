// Package repository is the in-memory store standing in for a database. It
// speaks domain types only, and it stores what it is given: deciding that a
// post needs a real author is the app layer's job, not storage's.
package repository

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syssam/graphql-go/examples/blog/internal/domain"
)

type Store struct {
	mu     sync.RWMutex
	users  map[string]*domain.User
	posts  []*domain.Post
	nextID int

	created broker
}

// NewStore returns a seeded store.
func NewStore() *Store {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	published := t0.Add(48 * time.Hour)
	return &Store{
		users: map[string]*domain.User{
			"1": {ID: "1", Name: "Alice", Email: "alice@example.com", Role: domain.RoleAdmin, CreatedAt: t0},
			"2": {ID: "2", Name: "Bob", Email: "bob@example.com", Role: domain.RoleUser, CreatedAt: t0.Add(time.Hour)},
		},
		posts: []*domain.Post{
			{ID: "10", Title: "Hello", Body: "First post", AuthorID: "1", Tags: []string{"intro"}, PublishedAt: &published},
			{ID: "11", Title: "Go generics", Body: "Type parameters in practice", AuthorID: "1", Tags: []string{"go", "generics"}},
			{ID: "12", Title: "Draft", Body: "Work in progress", AuthorID: "2", Tags: []string{}},
		},
		nextID: 13,
	}
}

func (s *Store) User(id string) *domain.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[id]
}

// UsersByIDs returns the users for ids in one lookup. Missing ids are omitted.
func (s *Store) UsersByIDs(ids []string) map[string]*domain.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*domain.User, len(ids))
	for _, id := range ids {
		if u := s.users[id]; u != nil {
			out[id] = u
		}
	}
	return out
}

func (s *Store) Users() []*domain.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usersLocked()
}

func (s *Store) usersLocked() []*domain.User {
	out := make([]*domain.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b *domain.User) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (s *Store) Post(id string) *domain.Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findPost(id)
}

func (s *Store) findPost(id string) *domain.Post {
	for _, p := range s.posts {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Posts lists posts matching the optional author and tag filters.
func (s *Store) Posts(authorID, tag *string, limit int) []*domain.Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Post, 0, len(s.posts))
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

// SearchUsers matches against an already-lowercased term.
func (s *Store) SearchUsers(lowered string) []*domain.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*domain.User{}
	for _, u := range s.usersLocked() {
		if strings.Contains(strings.ToLower(u.Name), lowered) {
			out = append(out, u)
		}
	}
	return out
}

// SearchPosts matches against an already-lowercased term.
func (s *Store) SearchPosts(lowered string) []*domain.Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*domain.Post{}
	for _, p := range s.posts {
		if strings.Contains(strings.ToLower(p.Title), lowered) {
			out = append(out, p)
		}
	}
	return out
}

// InsertPost stores a post as given, seeding included. It is how a test builds
// a record the normal path would refuse, such as a post whose author does not
// exist.
func (s *Store) InsertPost(p *domain.Post) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, p)
}

// CreatePost stores a new unpublished post. Nil tags become an empty slice
// because Post.tags is non-null.
func (s *Store) CreatePost(authorID, title, body string, tags []string) *domain.Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tags == nil {
		tags = []string{}
	}
	p := &domain.Post{ID: strconv.Itoa(s.nextID), Title: title, Body: body, AuthorID: authorID, Tags: tags}
	s.nextID++
	s.posts = append(s.posts, p)
	s.created.publish(p)
	return p
}

// UpdatePost applies a partial update and returns the post, or nil if there is
// no such post. It applies what it is given, including a clear the schema does
// not allow: refusing that is app's rule, and enforcing it here too would leave
// two half-answers to the same question.
func (s *Store) UpdatePost(id string, u domain.PostUpdate) *domain.Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.findPost(id)
	if p == nil {
		return nil
	}
	if u.Title.Present {
		if u.Title.Value == nil {
			p.Title = ""
		} else {
			p.Title = *u.Title.Value
		}
	}
	if u.Body.Present {
		if u.Body.Value == nil {
			p.Body = ""
		} else {
			p.Body = *u.Body.Value
		}
	}
	return p
}

func (s *Store) PostsCreated(ctx context.Context) <-chan *domain.Post {
	return s.created.subscribe(ctx)
}

// Subscribers reports how many subscriptions are open. It is exported for the
// leak test, which is the only thing that can observe a broker that fails to
// remove a cancelled subscriber.
func (s *Store) Subscribers() int { return s.created.subscribers() }
