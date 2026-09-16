package repository

import (
	"context"
	"testing"
	"time"

	"github.com/syssam/graphql-go/examples/blog/internal/domain"
)

func TestSeededStoreShape(t *testing.T) {
	s := NewStore()
	if u := s.User("1"); u == nil || u.Name != "Alice" || u.Role != domain.RoleAdmin {
		t.Fatalf("user 1 is %+v", u)
	}
	if got := len(s.Posts(nil, nil, 0)); got != 3 {
		t.Fatalf("seeded post count is %d, want 3", got)
	}
	author := "1"
	if got := len(s.Posts(&author, nil, 0)); got != 2 {
		t.Fatalf("alice has %d posts, want 2", got)
	}
	if got := s.Posts(nil, nil, 1); len(got) != 1 {
		t.Fatalf("limit ignored, got %d posts", len(got))
	}
}

func TestUpdatePostAppliesThreeValuedPatch(t *testing.T) {
	s := NewStore()
	title := "Final"
	s.UpdatePost("12", domain.PostUpdate{Title: domain.Present(&title)})
	if got := s.Post("12"); got.Title != "Final" || got.Body != "Work in progress" {
		t.Fatalf("absent body was not left alone: %+v", got)
	}
	s.UpdatePost("12", domain.PostUpdate{Body: domain.Present[string](nil)})
	if got := s.Post("12"); got.Body != "" {
		t.Fatalf("present-nil body did not clear, got %q", got.Body)
	}
}

func TestSubscriberIsRemovedOnCancel(t *testing.T) {
	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	s.PostsCreated(ctx)
	if n := s.Subscribers(); n != 1 {
		t.Fatalf("broker holds %d subscribers, want 1", n)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Subscribers() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("broker still holds %d subscribers after cancel", s.Subscribers())
}

func TestSlowSubscriberDoesNotBlockWriters(t *testing.T) {
	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.PostsCreated(ctx) // subscribe and never read

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			s.CreatePost("1", "spam", "b", nil)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber that stopped reading blocked the writers")
	}
}
