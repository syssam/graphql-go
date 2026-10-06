package howto

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/loader"
)

// countingAuthors answers from a map and counts round trips, which is the
// number an N+1 changes. The counters are atomic because resolvers call it
// from goroutines of their own.
type countingAuthors struct {
	rows map[string]*Author

	singles atomic.Int64 // calls to ByID
	batches atomic.Int64 // calls to ByIDs
	keys    atomic.Int64 // keys asked for across every ByIDs call
}

func (c *countingAuthors) ByID(_ context.Context, id string) (*Author, error) {
	c.singles.Add(1)
	return c.rows[id], nil
}

func (c *countingAuthors) ByIDs(_ context.Context, ids []string) (map[string]*Author, error) {
	c.batches.Add(1)
	c.keys.Add(int64(len(ids)))
	found := make(map[string]*Author, len(ids))
	for _, id := range ids {
		found[id] = c.rows[id]
	}
	return found, nil
}

// blogFixture is five posts by two authors: enough rows that one query per
// post and one query per request are different numbers, and enough repeats
// that the batch has to deduplicate.
func blogFixture() ([]*Post, *countingAuthors) {
	posts := []*Post{
		{ID: "1", Title: "Plans", AuthorID: "ada"},
		{ID: "2", Title: "Waves", AuthorID: "grace"},
		{ID: "3", Title: "Pools", AuthorID: "ada"},
		{ID: "4", Title: "Limits", AuthorID: "ada"},
		{ID: "5", Title: "Drains", AuthorID: "grace"},
	}
	authors := &countingAuthors{rows: map[string]*Author{
		"ada":   {ID: "ada", Name: "Ada"},
		"grace": {ID: "grace", Name: "Grace"},
	}}
	return posts, authors
}

const postsQuery = `{ posts { title author { name } } }`

const postsData = `{"posts":[` +
	`{"title":"Plans","author":{"name":"Ada"}},` +
	`{"title":"Waves","author":{"name":"Grace"}},` +
	`{"title":"Pools","author":{"name":"Ada"}},` +
	`{"title":"Limits","author":{"name":"Ada"}},` +
	`{"title":"Drains","author":{"name":"Grace"}}]}`

// runPosts executes the posts query and requires the full answer, so a count
// asserted afterwards is the cost of a correct response and not of a failed
// one.
func runPosts(t *testing.T, schema *graphql.Schema) {
	t.Helper()
	resp := graphql.NewExecutor(schema).Execute(context.Background(), &graphql.Request{Query: postsQuery})
	defer resp.Release()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %v", resp.Errors[0])
	}
	if got := string(resp.Data); got != postsData {
		t.Fatalf("data = %s\nwant   %s", got, postsData)
	}
}

func TestNaiveSchemaQueriesOncePerPost(t *testing.T) {
	posts, authors := blogFixture()
	schema, err := NewNaiveBlogSchema(posts, authors)
	if err != nil {
		t.Fatal(err)
	}
	runPosts(t, schema)

	if got := authors.singles.Load(); got != int64(len(posts)) {
		t.Fatalf("ByID ran %d times for %d posts, want one each", got, len(posts))
	}
}

func TestLoaderQueriesOncePerRequest(t *testing.T) {
	posts, authors := blogFixture()
	schema, err := NewBlogSchema(posts, authors)
	if err != nil {
		t.Fatal(err)
	}
	runPosts(t, schema)

	if got := authors.batches.Load(); got != 1 {
		t.Fatalf("ByIDs ran %d times for %d posts, want 1", got, len(posts))
	}
	if got := authors.keys.Load(); got != 2 {
		t.Fatalf("ByIDs was asked for %d keys, want the 2 distinct authors", got)
	}
}

// One Loader value serves every request, and each request gets a cache of its
// own: a second request asks the store again rather than reading what the
// first one loaded for somebody else.
func TestLoaderDoesNotCacheAcrossRequests(t *testing.T) {
	posts, authors := blogFixture()
	schema, err := NewBlogSchema(posts, authors)
	if err != nil {
		t.Fatal(err)
	}
	runPosts(t, schema)
	runPosts(t, schema)

	if got := authors.batches.Load(); got != 2 {
		t.Fatalf("ByIDs ran %d times over 2 requests, want 1 each", got)
	}
}

// The first mistake the page names, made on purpose: a loader built inside
// the resolver is a new one per post, so each has a queue of one key and the
// request is back to a query per post. Nothing errors and the answer is
// right, which is why only a count finds it.
func TestLoaderBuiltPerCallBatchesNothing(t *testing.T) {
	posts, authors := blogFixture()
	schema, err := newBlogSchema(posts,
		graphql.Resolve("author", func(ctx context.Context, p *Post) (*Author, error) {
			return loader.New(authors.ByIDs).Load(ctx, p.AuthorID)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	runPosts(t, schema)

	if got := authors.batches.Load(); got != int64(len(posts)) {
		t.Fatalf("ByIDs ran %d times for %d posts; a loader per call should batch nothing", got, len(posts))
	}
}

// The second: a lock held across Load. The resolver holding it is waiting in
// Load, its siblings are waiting for the lock, and a sibling waiting for
// anything but a Load counts as still running, so no batch is sent while
// anyone is waiting for one. The timeout is the only thing that ends the
// request; without one it would not end.
func TestLockHeldAcrossLoadHangsUntilTheTimeout(t *testing.T) {
	posts, authors := blogFixture()
	authorByID := loader.New(authors.ByIDs)
	var mu sync.Mutex
	schema, err := newBlogSchema(posts,
		graphql.Resolve("author", func(ctx context.Context, p *Post) (*Author, error) {
			mu.Lock()
			defer mu.Unlock()
			return authorByID.Load(ctx, p.AuthorID)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	exec := graphql.NewExecutor(schema, graphql.WithOperationTimeout(100*time.Millisecond))

	resp := exec.Execute(context.Background(), &graphql.Request{Query: postsQuery})
	defer resp.Release()

	if len(resp.Errors) == 0 || resp.Errors[0].Extensions["code"] != graphql.CodeOperationTimeout {
		t.Fatalf("errors = %v, want %s", resp.Errors, graphql.CodeOperationTimeout)
	}
	if got := string(resp.Data); got != "null" {
		t.Fatalf("data = %s; no author should have been loaded", got)
	}
}
