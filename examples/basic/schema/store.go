package schema

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/basic/graph/model"
)

// Store is an in-memory database seeded with a few records. GraphQL types
// come from generated models; author IDs are stored beside posts because
// Post.author is a resolver field, not a struct field.
type Store struct {
	mu         sync.RWMutex
	users      map[graphql.ID]*model.User
	posts      []*model.Post
	postAuthor map[graphql.ID]graphql.ID
	nextID     int
}

// NewStore returns a seeded store.
func NewStore() *Store {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	published := t0.Add(48 * time.Hour)
	return &Store{
		users: map[graphql.ID]*model.User{
			"1": {ID: "1", Name: "Alice", Email: "alice@example.com", Role: model.RoleAdmin, CreatedAt: t0},
			"2": {ID: "2", Name: "Bob", Email: "bob@example.com", Role: model.RoleUser, CreatedAt: t0.Add(time.Hour)},
		},
		posts: []*model.Post{
			{ID: "10", Title: "Hello", Body: "First post", Tags: []string{"intro"}, PublishedAt: &published},
			{ID: "11", Title: "Go generics", Body: "Type parameters in practice", Tags: []string{"go", "generics"}},
			{ID: "12", Title: "Draft", Body: "Work in progress", Tags: []string{}},
		},
		postAuthor: map[graphql.ID]graphql.ID{
			"10": "1",
			"11": "1",
			"12": "2",
		},
		nextID: 13,
	}
}

// User returns the user with the given id, or nil.
func (s *Store) User(id graphql.ID) *model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[id]
}

// UsersByIDs returns the users for ids in one lookup. Missing ids are omitted.
func (s *Store) UsersByIDs(ids []graphql.ID) map[graphql.ID]*model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[graphql.ID]*model.User, len(ids))
	for _, id := range ids {
		if u := s.users[id]; u != nil {
			out[id] = u
		}
	}
	return out
}

// Users lists all users ordered by id.
func (s *Store) Users() []*model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usersLocked()
}

func (s *Store) usersLocked() []*model.User {
	out := make([]*model.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b *model.User) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// Post returns the post with the given id, or nil.
func (s *Store) Post(id graphql.ID) *model.Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findPost(id)
}

func (s *Store) findPost(id graphql.ID) *model.Post {
	for _, p := range s.posts {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Store) authorID(postID graphql.ID) graphql.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.postAuthor[postID]
}

// Posts lists posts matching the optional author and tag filters.
func (s *Store) Posts(authorID *graphql.ID, tag *string, limit int) []*model.Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Post, 0, len(s.posts))
	for _, p := range s.posts {
		if authorID != nil && s.postAuthor[p.ID] != *authorID {
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
	for _, u := range s.usersLocked() {
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
func (s *Store) CreatePost(authorID graphql.ID, title, body string) (*model.Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[authorID] == nil {
		return nil, fmt.Errorf("author %q does not exist", authorID)
	}
	p := &model.Post{ID: graphql.ID(strconv.Itoa(s.nextID)), Title: title, Body: body, Tags: []string{}}
	s.nextID++
	s.posts = append(s.posts, p)
	s.postAuthor[p.ID] = authorID
	return p, nil
}

// UpdatePost applies a partial update. Unset fields are left untouched; a
// field set to null clears the value.
func (s *Store) UpdatePost(id graphql.ID, title, body graphql.Omittable[*string]) (*model.Post, error) {
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
