// Package app holds the use cases: the work that is neither storage nor
// protocol. Most of what is here forwards to the repository, which is the
// honest shape of a service layer over an in-memory store. What it buys is
// that a second caller -- an import job, an admin tool -- cannot reach the
// repository and skip the two rules below.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/syssam/graphql-go/examples/blog/internal/domain"
	"github.com/syssam/graphql-go/examples/blog/internal/repository"
)

type Service struct{ repo *repository.Store }

func New(repo *repository.Store) *Service { return &Service{repo: repo} }

func (s *Service) User(id string) *domain.User { return s.repo.User(id) }

func (s *Service) UsersByIDs(ids []string) map[string]*domain.User {
	return s.repo.UsersByIDs(ids)
}

func (s *Service) Users() []*domain.User { return s.repo.Users() }

func (s *Service) Post(id string) *domain.Post { return s.repo.Post(id) }

func (s *Service) Posts(authorID, tag *string, limit int) []*domain.Post {
	return s.repo.Posts(authorID, tag, limit)
}

// Search normalises the term once and fans it across both entities.
func (s *Service) Search(term string) ([]*domain.User, []*domain.Post) {
	lowered := strings.ToLower(term)
	return s.repo.SearchUsers(lowered), s.repo.SearchPosts(lowered)
}

func (s *Service) CreatePost(authorID, title, body string, tags []string) (*domain.Post, error) {
	if s.repo.User(authorID) == nil {
		return nil, fmt.Errorf("author %q does not exist", authorID)
	}
	return s.repo.CreatePost(authorID, title, body, tags), nil
}

// UpdatePost applies a partial update. Post.title is non-null in the schema, so
// an explicit null for it is a client error rather than a clear.
func (s *Service) UpdatePost(id string, u domain.PostUpdate) (*domain.Post, error) {
	if u.Title.Present && u.Title.Value == nil {
		return nil, errors.New("title cannot be cleared")
	}
	p := s.repo.UpdatePost(id, u)
	if p == nil {
		return nil, fmt.Errorf("post %q does not exist", id)
	}
	return p, nil
}

func (s *Service) PostsCreated(ctx context.Context) <-chan *domain.Post {
	return s.repo.PostsCreated(ctx)
}
