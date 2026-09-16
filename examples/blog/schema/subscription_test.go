package schema

import (
	"context"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
)

// newStoreExecutor is newExecutor with the store kept, so a test can drive
// mutations through the executor and still inspect the broker.
func newStoreExecutor(t *testing.T) (*Store, *graphql.Executor) {
	t.Helper()
	store := NewStore()
	s, err := NewSchema(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, graphql.NewExecutor(s)
}

func nextEvent(t *testing.T, events <-chan *graphql.Response) string {
	t.Helper()
	select {
	case resp, ok := <-events:
		if !ok {
			t.Fatal("stream closed, wanted an event")
		}
		if len(resp.Errors) > 0 {
			t.Fatalf("event errors: %v", resp.Errors)
		}
		out := string(resp.Data)
		resp.Release()
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return ""
	}
}

// TestCreatePostNotifiesSubscribers is the whole point of the example's
// subscription: a mutation on one connection reaches a subscriber on another.
func TestCreatePostNotifiesSubscribers(t *testing.T) {
	_, e := newStoreExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := e.Subscribe(ctx, &graphql.Request{
		Query: `subscription { postCreated { title author { name } } }`,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	execute(t, e, `mutation { createPost(authorId: "1", title: "Live", body: "b") { id } }`, "")

	// The author resolves through the DataLoader, so this also proves a
	// per-event request scope exists: a loader needs one to batch into.
	if got := nextEvent(t, events); got != `{"postCreated":{"title":"Live","author":{"name":"ALICE"}}}` {
		t.Fatalf("event = %s", got)
	}
}

func TestSubscriptionFilterByTag(t *testing.T) {
	_, e := newStoreExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := e.Subscribe(ctx, &graphql.Request{
		Query: `subscription { postCreatedWithTag(tag: "go") { title } }`,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// A post without the tag must not reach a filtered subscriber, and must
	// not stall the ones that follow it.
	execute(t, e, `mutation { createPost(authorId: "1", title: "Rust", body: "b", tags: ["rust"]) { id } }`, "")
	execute(t, e, `mutation { createPost(authorId: "1", title: "Untagged", body: "b") { id } }`, "")
	execute(t, e, `mutation { createPost(authorId: "1", title: "Go", body: "b", tags: ["go", "generics"]) { id } }`, "")

	if got := nextEvent(t, events); got != `{"postCreatedWithTag":{"title":"Go"}}` {
		t.Fatalf("event = %s, want only the tagged post", got)
	}
	select {
	case resp := <-events:
		t.Fatalf("filtered subscriber received an extra post: %s", resp.Data)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSubscriberIsReleasedOnCancel guards the leak: without removal on context
// end, every disconnected client would stay in the broker for the life of the
// process.
func TestSubscriberIsReleasedOnCancel(t *testing.T) {
	store, e := newStoreExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())

	if _, err := e.Subscribe(ctx, &graphql.Request{Query: `subscription { postCreated { id } }`}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if n := store.created.subscribers(); n != 1 {
		t.Fatalf("broker holds %d subscribers, want 1", n)
	}

	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.created.subscribers() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("broker still holds %d subscribers after cancellation", store.created.subscribers())
}

// TestSlowSubscriberDoesNotBlockMutations is the backpressure rule: a client
// that stops reading loses events rather than stalling every writer.
func TestSlowSubscriberDoesNotBlockMutations(t *testing.T) {
	store, _ := newStoreExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscribe but never read, so the buffer fills and then overflows.
	store.PostsCreated(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			if _, err := store.CreatePost("1", "spam", "b", nil); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber that stopped reading blocked the mutations")
	}
}
