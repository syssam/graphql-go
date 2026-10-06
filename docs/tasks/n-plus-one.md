# Stop an N+1 with a loader

A list of posts resolves each post's author, and the store is asked once per post. This
page turns that into one question per request and shows how to see the difference.

## The N+1

The schema, the Go types bound to it, and what the store can answer. A `Post` holds its
author's id, as a row of a posts table does, so the author is a second lookup.

```go
const blogSDL = `
type Author { id: ID! name: String! }
type Post { id: ID! title: String! author: Author! }
type Query { posts: [Post!]! }
`

type Author struct{ ID, Name string }

// Post carries AuthorID and not an *Author, because that is what a row of a
// posts table holds. The author is a second lookup.
type Post struct{ ID, Title, AuthorID string }

// Authors is what the store can answer: one row by its key, or many rows by
// their keys in one round trip. A loader needs the second.
type Authors interface {
	ByID(ctx context.Context, id string) (*Author, error)
	ByIDs(ctx context.Context, ids []string) (map[string]*Author, error)
}
```

This binding is the problem. `Resolve` binds a field that does I/O. The engine runs it once
for every post in the list, and every run is one `ByID`.

```go
// NewNaiveBlogSchema is the N+1: Post.author asks the store once per post.
func NewNaiveBlogSchema(posts []*Post, authors Authors) (*graphql.Schema, error) {
	return newBlogSchema(posts,
		graphql.Resolve("author", func(ctx context.Context, p *Post) (*Author, error) {
			return authors.ByID(ctx, p.AuthorID)
		}),
	)
}
```

## The fix

Build one loader around the batch query, and call `Load` where the resolver called the
store.

```go
// NewBlogSchema is the same schema with Post.author behind a loader.
func NewBlogSchema(posts []*Post, authors Authors) (*graphql.Schema, error) {
	// Built once and captured by the resolver. A loader keeps each request's
	// cache and queue apart by itself, so one value serves every request;
	// built inside the resolver it would be a new one per post.
	authorByID := loader.New(authors.ByIDs)

	return newBlogSchema(posts,
		graphql.Resolve("author", func(ctx context.Context, p *Post) (*Author, error) {
			return authorByID.Load(ctx, p.AuthorID)
		}),
	)
}
```

`Load` does not answer straight away. It queues its key and waits until no resolver in the
operation is still running. The loader then calls `ByIDs` once with every distinct key and
each `Load` returns its own row. A key missing from the returned map means "not found":
that `Load` returns the zero value and a nil error. Rows are cached for the rest of the
request and no longer.

Nothing else changes. `newBlogSchema` is the rest of the bindings, written as the quickstart
writes them, with this one field placed in `Object[Post]`. The whole file is
[`examples/howto/loader.go`](../../examples/howto/loader.go).

## Check it worked

Count round trips. This fake stands in for the store, and its `ByIDs` records how often it
was called and with how many keys.

```go
func (c *countingAuthors) ByIDs(_ context.Context, ids []string) (map[string]*Author, error) {
	c.batches.Add(1)
	c.keys.Add(int64(len(ids)))
	found := make(map[string]*Author, len(ids))
	for _, id := range ids {
		found[id] = c.rows[id]
	}
	return found, nil
}
```

The test queries `{ posts { title author { name } } }` over five posts by two authors.
`runPosts` executes it and requires the complete answer, so the counts are the cost of a
correct response.

```go
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
```

`TestNaiveSchemaQueriesOncePerPost` runs the same query against the first binding and
asserts five calls to `ByID`. Keep both: the pair fails if the difference is ever lost.

```sh
go test -race -run 'Naive|Loader|Lock' ./examples/howto/
```

## What goes wrong

- **Still one query per post, and nothing reports an error.** The request has more than
  one loader: it is built inside the resolver, or created on first use with
  `OperationContext.Get` then `Set`, where sibling resolvers all miss together and each
  installs its own. Build it once, with the schema. State that must be per request goes
  through `graphql.OperationFrom(ctx).GetOrSet`, which gives every sibling one value.
  `-race` cannot see the `Get`/`Set` pair; the `gqlvet` analyzer in `lint/` reports it.
- **The request hangs until its deadline.** A resolver held a mutex across `Load`. A batch
  is sent only when every resolver is finished or waiting in a `Load`, and a sibling
  waiting for that mutex counts as running. Hold no lock across `Load`, and set
  `graphql.WithOperationTimeout` so that a hang ends as an error.
- **The count differs from one run to the next.** Resolvers run on goroutines of their
  own, so whatever they share needs synchronizing, the test's counter included. Run the
  test with `-race` every time.

`TestLoaderBuiltPerCallBatchesNothing` and `TestLockHeldAcrossLoadHangsUntilTheTimeout`
make the first two mistakes on purpose.

## Next

- [`loader/loader.go`](../../loader/loader.go): the package comment says when a batch runs
  and what that costs; `NewMapped` reports failure per key, `WithMaxBatchSize` splits a batch.
- [`examples/blog`](../../examples/blog/internal/transport/graphql/resolver.go): the same
  loader in a layered service, where it lives on the resolver.
