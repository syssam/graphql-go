package app

import (
	"strings"
	"testing"

	"github.com/syssam/graphql-go/examples/blog/internal/domain"
	"github.com/syssam/graphql-go/examples/blog/internal/repository"
)

func newService() *Service { return New(repository.NewStore()) }

func TestCreatePostRejectsUnknownAuthor(t *testing.T) {
	_, err := newService().CreatePost("nope", "New", "Body", nil)
	if err == nil || err.Error() != `author "nope" does not exist` {
		t.Fatalf("got %v, want `author \"nope\" does not exist`", err)
	}
}

func TestUpdatePostRejectsClearingTitle(t *testing.T) {
	_, err := newService().UpdatePost("12", domain.PostUpdate{Title: domain.Present[string](nil)})
	if err == nil || err.Error() != "title cannot be cleared" {
		t.Fatalf("got %v, want `title cannot be cleared`", err)
	}
}

func TestUpdatePostRejectsUnknownPost(t *testing.T) {
	_, err := newService().UpdatePost("999", domain.PostUpdate{})
	if err == nil || !strings.Contains(err.Error(), `post "999" does not exist`) {
		t.Fatalf("got %v", err)
	}

	// A request that is invalid twice over reports the missing post, not the
	// title rule: that was the order before the layering and it is observable.
	_, err = newService().UpdatePost("999", domain.PostUpdate{Title: domain.Present[string](nil)})
	if err == nil || !strings.Contains(err.Error(), `post "999" does not exist`) {
		t.Fatalf("got %v, want the missing-post error to win", err)
	}
}

// TestRulesCannotBeBypassed is why this layer exists: the repository will
// happily store a post whose author does not exist, so the check has to sit
// somewhere every caller goes through.
func TestRulesCannotBeBypassed(t *testing.T) {
	repo := repository.NewStore()
	repo.CreatePost("nope", "smuggled", "b", nil)
	if p := repo.Posts(nil, nil, 0); len(p) != 4 {
		t.Fatalf("repository refused the write on its own; the rule is in the wrong layer")
	}
	if _, err := New(repo).CreatePost("nope", "smuggled", "b", nil); err == nil {
		t.Fatal("app accepted the write the repository should only reach through it")
	}
}

func TestSearchIsCaseInsensitiveAcrossBothTypes(t *testing.T) {
	users, posts := newService().Search("O")
	if len(users) != 1 || users[0].Name != "Bob" {
		t.Fatalf("users = %+v", users)
	}
	if len(posts) != 2 {
		t.Fatalf("posts = %+v", posts)
	}
}
