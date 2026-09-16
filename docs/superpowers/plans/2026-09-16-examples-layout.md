# examples/ Layout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `examples/{basic,echo,fiber}` with a tiny `quickstart/`, a layered `blog/` service, and two transport examples that serve `blog` instead of carrying their own copy of it.

**Architecture:** `blog` exposes exactly one public function, `NewSchema`, and hides `domain`, `repository`, `app` and the GraphQL adapter under `internal/`. `cmd/server`, `examples/echo` and `examples/fiber` each call it and wire their own transport. `quickstart` is the existing note board, de-duplicated out of `echo`/`fiber`, kept as hand-written bindings with no layers.

**Tech Stack:** Go 1.27, `graphql-go` root package, `go tool gqlc` codegen, `transport/gqlhttp`, `gqlsse`, `gqlws`, `gqlecho`, `gqlfiber`, `loader`.

**Spec:** `docs/superpowers/specs/2026-09-16-examples-layout-design.md`

## Global Constraints

- **Go 1.27 minimum.** Generic methods and `reflect.TypeFor` are in use.
- **`internal/domain` must not import `graphql-go` or the generated `graph`/`model` packages.** This is the point of the example; a change to the SDL must not change the type the repository stores.
- **The source SDL is `examples/blog/schema.graphql`.** `examples/blog/graph/schema/schema.graphql` is written by `gqlc` (`codegen/emit.go` copies each source into `<output>/schema/<basename>` for the `//go:embed`). Never edit the copy.
- **Test expectations are frozen.** Every moved test keeps its query and its expected response byte for byte. Only *how* a test reaches a fixture may change. A test whose expectation must be edited to pass means the refactor changed behaviour — stop and report, do not update the expectation.
- **`-race` is not optional** on any test run in this plan.
- **Comments explain why, not what. English only. No code-narrating comments.**
- **Commit messages:** imperative, lower-case type prefix (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`).
- **Attribution.** End every commit message with:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
  ```
- **The working tree has unrelated uncommitted changes**, someone else's work in progress on `loader.NewMapped` and plan cacheability: `context.go`, `exec.go`, `plan.go`, `plan_test.go`, `subscription.go`, `loader/`, `ext/otel/`, and **`README.md`**. Never `git add -A` or `git commit -a`. Stage only the exact paths each task names.
- **`README.md` is being edited concurrently.** That work is at line ~149 (the `loader.New` / `loader.NewMapped` row); Tasks 1 and 9 touch lines ~136 and ~177. Re-read the file immediately before editing it rather than trusting a line number from this plan, and stage `README.md` alone — never alongside a wildcard.

---

### Task 1: Rename `basic` to `blog` and lift the source SDL out of the generated tree

Pure move and rewire. No layering yet. The 12 existing tests must pass unchanged at the end of this task, which is what proves the move was mechanical.

**Files:**
- Move: `examples/basic/` → `examples/blog/` (whole directory, via `git mv`)
- Move: `examples/blog/schema/schema.graphql` → `examples/blog/schema.graphql`
- Modify: `examples/blog/gqlc.yaml`
- Modify: `examples/blog/main.go`, `examples/blog/schema/resolvers.go`, `examples/blog/schema/store.go`, `examples/blog/schema/broker.go`, `examples/blog/schema/schema_test.go` (import paths)
- Modify: `.github/workflows/ci.yml:55-58`
- Modify: `README.md:136`, `README.md:177`
- Modify: `CONTRIBUTING.md:67-71`
- Modify: `cmd/gqlc/README.md:40-41`
- Modify: `CLAUDE.md:56`
- Modify: `docs/module-layout.md:66`

**Interfaces:**
- Consumes: nothing.
- Produces: import path prefix `github.com/syssam/graphql-go/examples/blog`. The generated package is `github.com/syssam/graphql-go/examples/blog/graph`, exporting `NewSchema(r Resolver, opts ...graphql.SchemaOption)`, the `Resolver` interface, and args structs `NodeArgs`, `UserArgs`, `PostsArgs`, `SearchArgs`, `CreatePostArgs`, `UpdatePostArgs`, `UserPostsArgs`, `PostCreatedWithTagArgs`.

- [ ] **Step 1: Move the directory and the SDL**

```bash
cd "$(git rev-parse --show-toplevel)"
git mv examples/basic examples/blog
git mv examples/blog/schema/schema.graphql examples/blog/schema.graphql
```

- [ ] **Step 2: Point gqlc.yaml at the new source path and package**

Replace the whole of `examples/blog/gqlc.yaml` with:

```yaml
schema:
  - schema.graphql
output: graph
package: github.com/syssam/graphql-go/examples/blog/graph
nullableInputOmittable: true
models:
  Time: time.Time
```

- [ ] **Step 3: Rewrite the Go import paths**

```bash
cd "$(git rev-parse --show-toplevel)"
grep -rl 'examples/basic' examples/blog --include='*.go' \
  | xargs sed -i 's|examples/basic|examples/blog|g'
grep -rn 'examples/basic' examples/blog || echo "no stale import paths"
```

- [ ] **Step 4: Regenerate and confirm the generated SDL copy reappears in the right place**

```bash
cd examples/blog && go generate ./...
ls graph/schema/schema.graphql
```

Expected: `graph/schema/schema.graphql` exists. It is written by the generator from `examples/blog/schema.graphql`.

- [ ] **Step 5: Run the existing tests unchanged**

Run:
```bash
cd "$(git rev-parse --show-toplevel)" && go test -race -count=1 ./examples/... 2>&1 | tail -5
```
Expected: `ok github.com/syssam/graphql-go/examples/blog/schema`. All 12 tests pass with no source edits beyond import paths. If any expectation fails here, the move was not mechanical — stop.

- [ ] **Step 6: Update CI**

In `.github/workflows/ci.yml`, replace:

```yaml
          cd examples/basic && go generate ./...
          cd "$GITHUB_WORKSPACE"
          if ! git diff --quiet; then
            echo "generated code is stale; run go generate in examples/basic"
```

with:

```yaml
          cd examples/blog && go generate ./...
          cd "$GITHUB_WORKSPACE"
          if ! git diff --quiet; then
            echo "generated code is stale; run go generate in examples/blog"
```

- [ ] **Step 7: Update the five prose references**

- `README.md:136` and `README.md:177`: `[`examples/basic`](examples/basic)` → `[`examples/blog`](examples/blog)`
- `CONTRIBUTING.md:67`: "`examples/basic` is generated" → "`examples/blog` is generated"
- `CONTRIBUTING.md:71`: `cd examples/basic && go generate` → `cd examples/blog && go generate`
- `cmd/gqlc/README.md:40-41`: "The blog example is generated this way: `examples/basic/gqlc.yaml` writes `examples/basic/graph`, and `examples/basic/schema` implements `graph.Resolver`." → "The blog example is generated this way: `examples/blog/gqlc.yaml` writes `examples/blog/graph`, and `examples/blog/internal/transport/graphql` implements `graph.Resolver`."
- `CLAUDE.md:56`: `cd examples/basic && go generate` → `cd examples/blog && go generate`
- `docs/module-layout.md:66`: `examples/basic` → `examples/blog`

Do **not** touch dated files under `docs/superpowers/plans/` or `docs/superpowers/specs/` other than this plan. They record what was true when written.

- [ ] **Step 8: Verify nothing outside the history docs still says `examples/basic`**

```bash
cd "$(git rev-parse --show-toplevel)"
grep -rn 'examples/basic' --include='*.go' --include='*.md' --include='*.yml' --include='*.yaml' . \
  | grep -v '^./ref/' | grep -v '^./docs/superpowers/plans/2026-09-1[15]' \
  | grep -v '^./docs/superpowers/specs/2026-09-1[15]'
```
Expected: no output.

- [ ] **Step 9: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog .github/workflows/ci.yml README.md CONTRIBUTING.md \
        cmd/gqlc/README.md CLAUDE.md docs/module-layout.md
git commit -m "$(cat <<'EOF'
refactor: rename examples/basic to examples/blog

Also lifts the source SDL to examples/blog/schema.graphql. The copy under
graph/schema/ is written by gqlc for the //go:embed and is not editable;
sitting at a similar depth under a similar name, the two were impossible
to tell apart.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 2: Add `internal/domain`

New package, nothing imports it yet, so the tree stays green. It carries the three-valued `Optional` because the repository and app layers must express "absent / null / value" without importing `graphql.Omittable`.

**Files:**
- Create: `examples/blog/internal/domain/domain.go`
- Test: `examples/blog/internal/domain/domain_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `domain.Role` (`string`), `domain.RoleAdmin`, `domain.RoleUser`
  - `domain.User{ID, Name, Email string; Role Role; CreatedAt time.Time}`
  - `domain.Post{ID, Title, Body, AuthorID string; Tags []string; PublishedAt *time.Time}`
  - `domain.Optional[T]{Present bool; Value *T}`, `domain.Absent[T]() Optional[T]`, `domain.Present[T](v *T) Optional[T]`
  - `domain.PostUpdate{Title, Body Optional[string]}`

- [ ] **Step 1: Write the failing test**

Create `examples/blog/internal/domain/domain_test.go`:

```go
package domain

import "testing"

func TestOptionalDistinguishesAbsentFromNull(t *testing.T) {
	absent := Absent[string]()
	if absent.Present {
		t.Fatal("Absent reported itself as present")
	}

	cleared := Present[string](nil)
	if !cleared.Present || cleared.Value != nil {
		t.Fatalf("Present(nil) must be present and hold nil, got %+v", cleared)
	}

	v := "Final"
	set := Present(&v)
	if !set.Present || set.Value == nil || *set.Value != "Final" {
		t.Fatalf("Present(&v) must carry the value, got %+v", set)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd examples/blog && go test -race ./internal/domain/`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `examples/blog/internal/domain/domain.go`:

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd examples/blog && go test -race ./internal/domain/`
Expected: PASS.

- [ ] **Step 5: Confirm domain imports nothing it must not**

```bash
cd examples/blog && go list -deps ./internal/domain | grep -E 'graphql-go' || echo "clean: no graphql-go dependency"
```
Expected: `clean: no graphql-go dependency`.

- [ ] **Step 6: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog/internal/domain
git commit -m "$(cat <<'EOF'
feat: add the blog example's domain layer

Optional carries the absent/null/value distinction that UpdatePost needs,
so PATCH semantics reach the repository without graphql.Omittable.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 3: Add `internal/repository`

Stores domain types. No validation — rules go to `app` in Task 4. `InsertPost` exists because `TestNonNullBubbling` currently appends to `store.posts` directly, and after the split that field is another package's business.

**Files:**
- Create: `examples/blog/internal/repository/store.go`
- Create: `examples/blog/internal/repository/broker.go`
- Test: `examples/blog/internal/repository/store_test.go`

**Interfaces:**
- Consumes: `domain.User`, `domain.Post`, `domain.PostUpdate`, `domain.Optional`, `domain.Role*` from Task 2.
- Produces on `*repository.Store`:
  - `NewStore() *Store`
  - `User(id string) *domain.User`
  - `UsersByIDs(ids []string) map[string]*domain.User`
  - `Users() []*domain.User`
  - `Post(id string) *domain.Post`
  - `Posts(authorID, tag *string, limit int) []*domain.Post`
  - `SearchUsers(lowered string) []*domain.User`
  - `SearchPosts(lowered string) []*domain.Post`
  - `InsertPost(p *domain.Post)`
  - `CreatePost(authorID, title, body string, tags []string) *domain.Post`
  - `UpdatePost(id string, u domain.PostUpdate) *domain.Post`
  - `PostsCreated(ctx context.Context) <-chan *domain.Post`
  - `Subscribers() int`

- [ ] **Step 1: Write the failing test**

Create `examples/blog/internal/repository/store_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd examples/blog && go test -race ./internal/repository/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the broker**

Create `examples/blog/internal/repository/broker.go`:

```go
package repository

import (
	"context"
	"sync"

	"github.com/syssam/graphql-go/examples/blog/internal/domain"
)

// broker fans each created post out to every open subscription.
//
// Two rules make the difference between a demo and something that survives a
// real client. A subscriber that is not keeping up is dropped rather than
// allowed to block: a mutation must never wait on a slow WebSocket, so the
// send is non-blocking and a full buffer loses the event. And a subscriber is
// removed when its context ends, which is how the transports report both an
// unsubscribe and a disconnect; without that the map grows for the life of the
// process.
type broker struct {
	mu   sync.Mutex
	next int
	subs map[int]chan *domain.Post
}

func (b *broker) subscribe(ctx context.Context) <-chan *domain.Post {
	ch := make(chan *domain.Post, 16)

	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[int]chan *domain.Post)
	}
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.remove(id)
	}()
	return ch
}

func (b *broker) remove(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Closing under the same lock publish holds is what makes a send to a
	// closed channel impossible.
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

func (b *broker) publish(p *domain.Post) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (b *broker) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
```

- [ ] **Step 4: Write the store**

Create `examples/blog/internal/repository/store.go`:

```go
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
// no such post. Rejecting a clear the schema does not allow happens in app.
func (s *Store) UpdatePost(id string, u domain.PostUpdate) *domain.Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.findPost(id)
	if p == nil {
		return nil
	}
	if u.Title.Present && u.Title.Value != nil {
		p.Title = *u.Title.Value
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd examples/blog && go test -race -count=1 ./internal/...`
Expected: PASS for both `internal/domain` and `internal/repository`.

- [ ] **Step 6: Prove the leak test can fail**

Temporarily comment out the body of the goroutine in `broker.subscribe`:

```go
	go func() {
		<-ctx.Done()
		// b.remove(id)
	}()
```

Run: `cd examples/blog && go test -race -run TestSubscriberIsRemovedOnCancel ./internal/repository/`
Expected: **FAIL** with "broker still holds 1 subscribers after cancel". Then restore the line and re-run to confirm PASS.

A leak test that cannot fail is the failure mode this repository has hit before. Do not skip this step.

- [ ] **Step 7: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog/internal/repository
git commit -m "$(cat <<'EOF'
feat: add the blog example's repository layer

Stores domain types and nothing else. Subscribers() is exported because
the leak test is the only thing that can see a broker that fails to drop
a cancelled subscriber.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 4: Add `internal/app`

The rules live here. Both error strings are asserted verbatim by tests moved in Task 6, so they must match character for character.

**Files:**
- Create: `examples/blog/internal/app/service.go`
- Test: `examples/blog/internal/app/service_test.go`

**Interfaces:**
- Consumes: `*repository.Store` and its methods from Task 3; `domain.*` from Task 2.
- Produces on `*app.Service`:
  - `app.New(repo *repository.Store) *Service`
  - `User(id string) *domain.User`
  - `UsersByIDs(ids []string) map[string]*domain.User`
  - `Users() []*domain.User`
  - `Post(id string) *domain.Post`
  - `Posts(authorID, tag *string, limit int) []*domain.Post`
  - `Search(term string) ([]*domain.User, []*domain.Post)`
  - `CreatePost(authorID, title, body string, tags []string) (*domain.Post, error)`
  - `UpdatePost(id string, u domain.PostUpdate) (*domain.Post, error)`
  - `PostsCreated(ctx context.Context) <-chan *domain.Post`

- [ ] **Step 1: Write the failing test**

Create `examples/blog/internal/app/service_test.go`:

```go
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
}

func TestSearchLowercasesOnce(t *testing.T) {
	users, posts := newService().Search("O")
	if len(users) != 1 || users[0].Name != "Bob" {
		t.Fatalf("users = %+v", users)
	}
	if len(posts) != 2 {
		t.Fatalf("posts = %+v", posts)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd examples/blog && go test -race ./internal/app/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the implementation**

Create `examples/blog/internal/app/service.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd examples/blog && go test -race -count=1 ./internal/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog/internal/app
git commit -m "$(cat <<'EOF'
feat: add the blog example's app layer

Holds the two rules that must not be bypassable: a post needs a real
author, and title cannot be cleared because the schema declares it
non-null. The repository stores what it is given.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 5: Add the GraphQL adapter and the composition root

`internal/transport/graphql` implements `graph.Resolver` over domain types; `blog.go` composes everything and is the only public surface.

**This task adds only.** The old `examples/blog/schema/` package stays in place and keeps working, so this commit is green and the 12 original tests still pass against the *old* implementation. Task 6 does the switch-over and the deletion in one green commit. Do not delete anything here — a commit whose test package does not compile is not independently reviewable.

**Files:**
- Create: `examples/blog/internal/transport/graphql/resolver.go`
- Create: `examples/blog/blog.go`

**Interfaces:**
- Consumes: `*app.Service` from Task 4; `graph.NewSchema`, `graph.Resolver` and the args structs from Task 1.
- Produces:
  - `resolver.New(svc *app.Service) *resolver.Resolver`, satisfying `graph.Resolver`
  - `blog.NewSchema(opts ...graphql.SchemaOption) (*graphql.Schema, error)`
  - unexported `blog.newSchema(repo *repository.Store, opts ...graphql.SchemaOption) (*graphql.Schema, error)`

- [ ] **Step 1: Write the adapter**

Create `examples/blog/internal/transport/graphql/resolver.go`:

```go
// Package graphql adapts the domain to the generated GraphQL bindings. It is
// the only package that knows both domain types and generated model types.
package graphql

import (
	"context"
	"fmt"
	"slices"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/blog/graph"
	"github.com/syssam/graphql-go/examples/blog/graph/model"
	"github.com/syssam/graphql-go/examples/blog/internal/app"
	"github.com/syssam/graphql-go/examples/blog/internal/domain"
	"github.com/syssam/graphql-go/loader"
)

// Resolver implements graph.Resolver.
type Resolver struct {
	svc     *app.Service
	authors *loader.Loader[graphql.ID, *model.User]
}

var _ graph.Resolver = (*Resolver)(nil)

// New builds the resolver. The DataLoader lives here rather than in app
// because it exists to batch the N+1 that GraphQL's field-at-a-time resolution
// creates; nothing outside this adapter would ever want one.
func New(svc *app.Service) *Resolver {
	r := &Resolver{svc: svc}
	r.authors = loader.New(func(_ context.Context, ids []graphql.ID) (map[graphql.ID]*model.User, error) {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = string(id)
		}
		found := svc.UsersByIDs(keys)
		out := make(map[graphql.ID]*model.User, len(found))
		for k, u := range found {
			out[graphql.ID(k)] = toUser(u)
		}
		return out, nil
	})
	return r
}

func toUser(u *domain.User) *model.User {
	if u == nil {
		return nil
	}
	return &model.User{
		ID:        graphql.ID(u.ID),
		Name:      u.Name,
		Email:     u.Email,
		Role:      model.Role(u.Role),
		CreatedAt: u.CreatedAt,
	}
}

func toPost(p *domain.Post) *model.Post {
	if p == nil {
		return nil
	}
	return &model.Post{
		ID:          graphql.ID(p.ID),
		Title:       p.Title,
		Body:        p.Body,
		Tags:        p.Tags,
		PublishedAt: p.PublishedAt,
	}
}

func toPosts(ps []*domain.Post) []*model.Post {
	out := make([]*model.Post, len(ps))
	for i, p := range ps {
		out[i] = toPost(p)
	}
	return out
}

// optional converts the GraphQL layer's three-valued Omittable into the
// domain's own, so nothing below this package imports graphql.Omittable.
func optional(o graphql.Omittable[*string]) domain.Optional[string] {
	v, ok := o.ValueOK()
	if !ok {
		return domain.Absent[string]()
	}
	return domain.Present(v)
}

func (r *Resolver) Node(_ context.Context, a graph.NodeArgs) (any, error) {
	if u := r.svc.User(string(a.ID)); u != nil {
		return toUser(u), nil
	}
	if p := r.svc.Post(string(a.ID)); p != nil {
		return toPost(p), nil
	}
	return nil, nil
}

func (r *Resolver) User(_ context.Context, a graph.UserArgs) (*model.User, error) {
	return toUser(r.svc.User(string(a.ID))), nil
}

func (r *Resolver) Users(context.Context) ([]*model.User, error) {
	users := r.svc.Users()
	out := make([]*model.User, len(users))
	for i, u := range users {
		out[i] = toUser(u)
	}
	return out, nil
}

func (r *Resolver) Posts(_ context.Context, a graph.PostsArgs) ([]*model.Post, error) {
	if a.Filter == nil {
		return toPosts(r.svc.Posts(nil, nil, 0)), nil
	}
	var authorID *string
	if v, ok := a.Filter.AuthorID.ValueOK(); ok && v != nil {
		s := string(*v)
		authorID = &s
	}
	return toPosts(r.svc.Posts(authorID, a.Filter.Tag.Or(nil), 0)), nil
}

func (r *Resolver) Search(_ context.Context, a graph.SearchArgs) ([]any, error) {
	users, posts := r.svc.Search(a.Term)
	out := make([]any, 0, len(users)+len(posts))
	for _, u := range users {
		out = append(out, toUser(u))
	}
	for _, p := range posts {
		out = append(out, toPost(p))
	}
	return out, nil
}

func (r *Resolver) CreatePost(_ context.Context, a graph.CreatePostArgs) (*model.Post, error) {
	p, err := r.svc.CreatePost(string(a.AuthorID), a.Title, a.Body, a.Tags)
	if err != nil {
		return nil, err
	}
	return toPost(p), nil
}

func (r *Resolver) UpdatePost(_ context.Context, a graph.UpdatePostArgs) (*model.Post, error) {
	p, err := r.svc.UpdatePost(string(a.ID), domain.PostUpdate{
		Title: optional(a.Input.Title),
		Body:  optional(a.Input.Body),
	})
	if err != nil {
		return nil, err
	}
	return toPost(p), nil
}

func (r *Resolver) UserPosts(_ context.Context, u *model.User, a graph.UserPostsArgs) ([]*model.Post, error) {
	limit := 0
	if a.First != nil {
		limit = *a.First
	}
	id := string(u.ID)
	return toPosts(r.svc.Posts(&id, nil, limit)), nil
}

// PostAuthor pays for the domain/model split. model.Post has no AuthorID,
// because Post.author is a resolver field in the SDL, so the author id has to
// be fetched back out of the store by post id before the loader can batch the
// user lookup. Binding Object[domain.Post] directly would hand it over free.
func (r *Resolver) PostAuthor(ctx context.Context, p *model.Post) (*model.User, error) {
	d := r.svc.Post(string(p.ID))
	if d == nil {
		return nil, fmt.Errorf("post %q does not exist", p.ID)
	}
	u, err := r.authors.Load(ctx, graphql.ID(d.AuthorID))
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("author %q of post %q is missing", d.AuthorID, p.ID)
	}
	return u, nil
}

// PostCreated streams posts as CreatePost stores them.
func (r *Resolver) PostCreated(ctx context.Context) (<-chan *model.Post, error) {
	return r.stream(ctx, func(*domain.Post) bool { return true }), nil
}

// PostCreatedWithTag is the filtered form. The filter runs in a goroutine
// between the source and the subscriber rather than inside the broker, so one
// client's predicate cannot slow down publishing to the others.
func (r *Resolver) PostCreatedWithTag(ctx context.Context, a graph.PostCreatedWithTagArgs) (<-chan *model.Post, error) {
	return r.stream(ctx, func(p *domain.Post) bool { return slices.Contains(p.Tags, a.Tag) }), nil
}

func (r *Resolver) stream(ctx context.Context, keep func(*domain.Post) bool) <-chan *model.Post {
	src := r.svc.PostsCreated(ctx)
	out := make(chan *model.Post)
	go func() {
		defer close(out)
		for p := range src {
			if !keep(p) {
				continue
			}
			select {
			case out <- toPost(p):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
```

- [ ] **Step 2: Write the composition root**

Create `examples/blog/blog.go`:

```go
// Package blog is the composition root for the layered example. NewSchema is
// the only thing it exports, so every transport -- net/http, Echo, Fiber --
// wires the same schema without reaching into internal/.
package blog

//go:generate go tool gqlc -config gqlc.yaml

import (
	"context"
	"fmt"
	"strings"
	"time"

	graphql "github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/blog/graph"
	"github.com/syssam/graphql-go/examples/blog/internal/app"
	"github.com/syssam/graphql-go/examples/blog/internal/repository"
	resolver "github.com/syssam/graphql-go/examples/blog/internal/transport/graphql"
)

// NewSchema builds the schema over a freshly seeded store.
func NewSchema(opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	return newSchema(repository.NewStore(), opts...)
}

// newSchema keeps the store visible to this package's tests, which drive the
// executor and inspect the broker at the same time.
func newSchema(repo *repository.Store, opts ...graphql.SchemaOption) (*graphql.Schema, error) {
	all := append([]graphql.SchemaOption{
		graphql.Scalar("Time", marshalTime, unmarshalTime),
		graphql.Directive("upper", func(next graphql.FieldFunc) graphql.FieldFunc {
			return func(ctx context.Context, parent, args any) (any, error) {
				v, err := next(ctx, parent, args)
				if s, ok := v.(string); ok {
					return strings.ToUpper(s), err
				}
				return v, err
			}
		}),
	}, opts...)
	return graph.NewSchema(resolver.New(app.New(repo)), all...)
}

func marshalTime(w *graphql.Writer, t time.Time) error {
	w.String(t.UTC().Format(time.RFC3339Nano))
	return nil
}

func unmarshalTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("Time must be an RFC 3339 string, got %T", v)
	}
	return time.Parse(time.RFC3339Nano, s)
}
```

- [ ] **Step 3: Verify the adapter satisfies the generated interface**

Run: `cd "$(git rev-parse --show-toplevel)" && go build ./examples/...`
Expected: builds. The `var _ graph.Resolver = (*Resolver)(nil)` line makes a
missing or mis-typed method a compile error here rather than a runtime surprise.

This is the check CLAUDE.md exists for: generated bindings that read correctly
and do not compile have shipped twice in this repository, and reading never
caught either.

- [ ] **Step 4: Confirm the old path still works, untouched**

Run: `cd "$(git rev-parse --show-toplevel)" && go test -race -count=1 ./examples/...`
Expected: PASS. The 12 original tests still run against `examples/blog/schema`,
which this task has not modified. The new adapter is compiled but not yet wired.

- [ ] **Step 5: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog/internal/transport examples/blog/blog.go
git commit -m "$(cat <<'EOF'
feat: add the blog example's GraphQL adapter and composition root

internal/transport/graphql implements graph.Resolver over domain types and
owns the author DataLoader; blog.NewSchema is the only public surface, so a
transport example can wire the schema without reaching into internal/.

Not yet wired: the existing schema package still serves main, so this
commit is green on its own. The switch-over is one commit away.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 6: Switch over to the new layer and move the 12 tests

The safety net, and the moment the old implementation goes away. Expectations are frozen; only fixture access changes. One commit, green at both ends.

**Files:**
- Create: `examples/blog/blog_test.go` (from `examples/blog/schema/schema_test.go`)
- Create: `examples/blog/subscription_test.go` (from `examples/blog/schema/subscription_test.go`)
- Delete: `examples/blog/schema/` entirely
- Modify: `examples/blog/main.go`

**Interfaces:**
- Consumes: `blog.newSchema` and `blog.NewSchema` from Task 5; `repository.NewStore`, `repository.Store.InsertPost`, `repository.Store.Subscribers`, `repository.Store.CreatePost`, `repository.Store.PostsCreated` from Task 3; `domain.Post` from Task 2.
- Produces: nothing downstream.

- [ ] **Step 1: Point main.go at the composition root and drop the old package**

In `examples/blog/main.go`, replace the import

```go
	"github.com/syssam/graphql-go/examples/blog/schema"
```

with

```go
	blog "github.com/syssam/graphql-go/examples/blog"
```

and replace

```go
	s, err := schema.NewSchema(schema.NewStore())
```

with

```go
	s, err := blog.NewSchema()
```

Then remove the old implementation, keeping its two test files by moving them
out first:

```bash
cd "$(git rev-parse --show-toplevel)"
git mv examples/blog/schema/schema_test.go examples/blog/blog_test.go
git mv examples/blog/schema/subscription_test.go examples/blog/subscription_test.go
git rm examples/blog/schema/resolvers.go examples/blog/schema/store.go examples/blog/schema/broker.go
rmdir examples/blog/schema 2>/dev/null || true
```

- [ ] **Step 2: Fix the moved tests' package and helpers**

In `examples/blog/blog_test.go`:
- change `package schema` to `package blog`
- replace the model import `"github.com/syssam/graphql-go/examples/blog/graph/model"` with
  `"github.com/syssam/graphql-go/examples/blog/internal/domain"` and
  `"github.com/syssam/graphql-go/examples/blog/internal/repository"`
- replace the `newExecutor` helper with:

```go
func newExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	s, err := newSchema(repository.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}
```

- replace the body of `TestNonNullBubbling` up to the query with:

```go
func TestNonNullBubbling(t *testing.T) {
	repo := repository.NewStore()
	repo.InsertPost(&domain.Post{ID: "99", Title: "Orphan", AuthorID: "missing", Tags: []string{}})
	s, err := newSchema(repo)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
```

Everything from the `// author is non-null` comment onward, including the
expected response string, stays exactly as it is.

In `examples/blog/subscription_test.go`:
- change `package schema` to `package blog`
- add the import `"github.com/syssam/graphql-go/examples/blog/internal/repository"`
- replace the `newStoreExecutor` helper with:

```go
func newStoreExecutor(t *testing.T) (*repository.Store, *graphql.Executor) {
	t.Helper()
	repo := repository.NewStore()
	s, err := newSchema(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, graphql.NewExecutor(s)
}
```

- in `TestSubscriberIsReleasedOnCancel`, replace the three occurrences of
  `store.created.subscribers()` with `store.Subscribers()`
- in `TestSlowSubscriberDoesNotBlockMutations`, replace

```go
			if _, err := store.CreatePost("1", "spam", "b", nil); err != nil {
				return
			}
```

with

```go
			store.CreatePost("1", "spam", "b", nil)
```

because the repository's `CreatePost` no longer returns an error — the author
check moved to `app`. Its `store.PostsCreated(ctx)` call is unchanged.

- [ ] **Step 3: Run all 12 tests**

Run: `cd examples/blog && go test -race -count=1 ./... 2>&1 | tail -10`
Expected: PASS, with **no expected-response string edited**. If an expectation
fails, the refactor changed behaviour — stop and report rather than adjusting
the expectation.

- [ ] **Step 4: Confirm the count**

```bash
cd examples/blog && go test -race -count=1 -v . 2>&1 | grep -c '^--- PASS'
```
Expected: `12`.

- [ ] **Step 5: Confirm the old package is gone and nothing references it**

```bash
cd "$(git rev-parse --show-toplevel)"
test ! -d examples/blog/schema && echo "old package removed"
grep -rn 'examples/blog/schema"' --include='*.go' . | grep -v '^./ref/' || echo "no references"
```
Expected: `old package removed` and `no references`.

- [ ] **Step 6: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog
git commit -m "$(cat <<'EOF'
refactor: serve the blog example through the layered stack

Switches main to blog.NewSchema and removes the old schema package.
Test queries and expected responses are unchanged; only fixture access
moved. TestNonNullBubbling seeds its orphan through repository.InsertPost
rather than appending to an unexported slice.

Queries and expected responses are unchanged; only fixture access moved.
TestNonNullBubbling seeds its orphan through repository.InsertPost rather
than appending to an unexported slice.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 7: Move `main.go` under `cmd/server`

**Files:**
- Move: `examples/blog/main.go` → `examples/blog/cmd/server/main.go`

**Interfaces:**
- Consumes: `blog.NewSchema` from Task 5.
- Produces: the binary `./examples/blog/cmd/server`.

- [ ] **Step 1: Move the file**

```bash
cd "$(git rev-parse --show-toplevel)"
mkdir -p examples/blog/cmd/server
git mv examples/blog/main.go examples/blog/cmd/server/main.go
```

- [ ] **Step 2: Remove the now-duplicated go:generate directive**

`blog.go` carries `//go:generate go tool gqlc -config gqlc.yaml`. Delete the
line from `examples/blog/cmd/server/main.go` — from `cmd/server` the relative
config path would not resolve.

- [ ] **Step 3: Confirm generate still works from the example root**

```bash
cd examples/blog && go generate ./... && git -C "$(git rev-parse --show-toplevel)" diff --quiet -- examples/blog && echo "generate is idempotent and the tree is clean"
```
Expected: `generate is idempotent and the tree is clean`.

- [ ] **Step 4: Build and test**

Run: `cd "$(git rev-parse --show-toplevel)" && go build ./examples/... && go test -race -count=1 ./examples/...`
Expected: builds, 12 tests plus the layer tests pass.

- [ ] **Step 5: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/blog
git commit -m "$(cat <<'EOF'
refactor: move the blog example's server under cmd/server

Leaves room for the transport examples to be entrypoints over the same
service rather than copies of it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 8: Extract `quickstart` and point `echo`/`fiber` at `blog`

Deletes the duplication. `examples/echo/resolvers.go` and
`examples/fiber/resolvers.go` are byte-identical today; one copy becomes
`examples/quickstart/notes.go` and both transports serve `blog` instead.

**Files:**
- Create: `examples/quickstart/main.go`
- Move: `examples/echo/resolvers.go` → `examples/quickstart/notes.go`
- Move: `examples/echo/schema.graphql` → `examples/quickstart/schema.graphql`
- Delete: `examples/fiber/resolvers.go`, `examples/fiber/schema.graphql`
- Modify: `examples/echo/main.go`, `examples/fiber/main.go`

**Interfaces:**
- Consumes: `blog.NewSchema` from Task 5.
- Produces: `./examples/quickstart`, `./examples/echo`, `./examples/fiber` binaries.

- [ ] **Step 1: Move one copy of the note board to quickstart**

```bash
cd "$(git rev-parse --show-toplevel)"
mkdir -p examples/quickstart
git mv examples/echo/resolvers.go examples/quickstart/notes.go
git mv examples/echo/schema.graphql examples/quickstart/schema.graphql
git rm examples/fiber/resolvers.go examples/fiber/schema.graphql
```

- [ ] **Step 2: Write the quickstart server**

Create `examples/quickstart/main.go`:

```go
// Command quickstart is the smallest thing in this repository that runs: one
// SDL file, hand-written bindings, no code generation and no layers.
//
//	/graphql         queries and mutations over HTTP
//	/graphql/stream  Server-Sent Events
//
// Subscribe to noteCreated on the streaming endpoint, then run the createNote
// mutation against /graphql to see the event arrive. For what this looks like
// once a service grows a database and a wire format, read examples/blog.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/transport/gqlhttp"
	"github.com/syssam/graphql-go/transport/gqlsse"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}

// run holds every deferred cleanup, so that main can exit non-zero without
// skipping any of it.
func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	s, err := newSchema(newStore())
	if err != nil {
		return fmt.Errorf("building schema: %w", err)
	}

	exec := graphql.NewExecutor(s)
	mux := http.NewServeMux()
	mux.Handle("/graphql", gqlhttp.New(exec))
	mux.Handle("/graphql/stream", gqlsse.New(exec))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("serving GraphQL", "addr", *addr, "http", "/graphql", "sse", "/graphql/stream")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

- [ ] **Step 3: Update the comment in notes.go that points at the old layout**

In `examples/quickstart/notes.go`, the doc comment on `newSchema` reads
"see examples/basic and cmd/gqlc for the codegen path this example deliberately
skips, to stay copyable as a single small package." Replace `examples/basic`
with `examples/blog`.

- [ ] **Step 4: Point both transport examples at blog**

In **`examples/echo/main.go`**:
- change the first doc line from "Command echo serves the example note-board
  schema through Echo v5, on every transport:" to "Command echo serves the
  examples/blog schema through Echo v5, on every transport:"
- change the closing doc line "Subscribe to noteCreated on either streaming
  endpoint, then run the createNote mutation against /graphql to see the event
  arrive." to "Subscribe to postCreated on either streaming endpoint, then run
  the createPost mutation against /graphql to see the event arrive."
- add the import `blog "github.com/syssam/graphql-go/examples/blog"`
- replace `s, err := newSchema(newStore())` with `s, err := blog.NewSchema()`

Make the identical four changes in **`examples/fiber/main.go`**, reading
"through Fiber v3" in the first line.

Change nothing else in either file. Every other comment in them — why `e.Any`
and `app.All` rather than per-method routes, why `BodyLimit` is set alongside
`gqlfiber`'s own limit, why `ShutdownWithTimeout` cannot take a bare context,
why the strict origin default is left alone — is the reason these examples
exist. Preserve all of it verbatim.

- [ ] **Step 5: Build everything**

Run: `cd "$(git rev-parse --show-toplevel)" && go build ./examples/...`
Expected: builds, with no `examples/echo/resolvers.go` or
`examples/fiber/resolvers.go` remaining.

- [ ] **Step 6: Confirm the duplication is gone**

```bash
cd "$(git rev-parse --show-toplevel)"
find examples -name 'schema.graphql' -o -name 'resolvers.go' | sort
```
Expected exactly these three, and no `resolvers.go` at all:
```
examples/blog/graph/schema/schema.graphql
examples/blog/schema.graphql
examples/quickstart/schema.graphql
```
The first is the generator's copy under `graph/`, the second is its source, and
the third is the quickstart's. Two SDL files describing the *same* schema in two
different example directories is the duplication this task removes; a source and
its generated copy is not.

- [ ] **Step 7: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/quickstart examples/echo examples/fiber
git commit -m "$(cat <<'EOF'
refactor: de-duplicate the note board into examples/quickstart

examples/echo and examples/fiber carried byte-identical copies of a
162-line note board and its SDL. One copy becomes the quickstart; both
transports now serve examples/blog, which makes one schema over three
frameworks demonstrate the transport independence the README asserts.

Reverses the self-contained-examples decision in
docs/superpowers/plans/2026-09-15-echo-fiber-transports.md deliberately:
the copyable-starting-point role is served better by quickstart, which is
smaller and has no framework in it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

### Task 9: READMEs, full gate, and serving all three for real

**Files:**
- Create: `examples/README.md`
- Modify: `README.md` (the `examples/blog` sentence at line ~136)

**Interfaces:**
- Consumes: everything above.
- Produces: nothing downstream.

- [ ] **Step 1: Write `examples/README.md`**

```markdown
# Examples

| Directory | Read it for |
|---|---|
| [`quickstart`](quickstart) | The smallest thing that runs: one SDL file, hand-written bindings, no codegen, no layers. |
| [`blog`](blog) | A layered service: generated bindings, a domain isolated from the wire format, a DataLoader, subscriptions. |
| [`echo`](echo) | Serving `blog` through Echo v5 — routing and shutdown wiring only. |
| [`fiber`](fiber) | Serving `blog` through Fiber v3 — routing and shutdown wiring only. |

`echo` and `fiber` build the same schema `blog/cmd/server` does. One schema
over three frameworks is the point: nothing in `blog` knows which transport is
serving it.

## Two ways to bind Go types

`quickstart` binds its own type straight to the schema:

    graphql.Object[Note]("Note", graphql.Field("id", ...))

`blog` does not. Its `internal/domain` types never reach the GraphQL layer;
`internal/transport/graphql` maps them to the generated models at the boundary,
so editing the SDL cannot change the type the repository stores.

**The library does not require this.** `Object[domain.User]` works, and for a
small service it is the better trade. `blog` pays for the separation to show
what it costs as well as what it buys — and it does cost. `Post.author` is a
resolver field, so the generated `model.Post` has no `AuthorID`, and
`PostAuthor` has to fetch the post back out of the store to recover an author id
the domain object was already holding, before the DataLoader can batch anything.
Binding the domain type directly would hand it over for free.

## Layers in `blog`

| Package | Holds |
|---|---|
| `internal/domain` | Entities. Imports neither `graphql-go` nor the generated models. |
| `internal/repository` | Storage. Stores what it is given. |
| `internal/app` | The rules: a post needs a real author; `title` cannot be cleared. |
| `internal/transport/graphql` | Implements the generated `Resolver`, maps domain to model, owns the DataLoader. |
| `graph/` | Generated by `gqlc`. Do not edit. |

Most of `app` forwards to the repository, which is the honest shape of a service
layer over an in-memory store. It earns its place by being the only path to the
two rules — a repository will happily store a post whose author does not exist.

`blog` exports one function, `NewSchema`. Everything else is under `internal/`,
which is what lets `examples/echo` use the schema and not its internals.

## Editing the schema

`blog/schema.graphql` is the source. `blog/graph/schema/schema.graphql` is a
copy `gqlc` writes for the `//go:embed`; editing it is undone by the next
`go generate`.

    cd examples/blog && go generate

## Running

    go run ./examples/quickstart          # :8080 /graphql, /graphql/stream
    go run ./examples/blog/cmd/server     # :8080 + /graphql/ws
    go run ./examples/echo                # same schema, Echo v5
    go run ./examples/fiber               # same schema, Fiber v3
```

- [ ] **Step 2: Point the root README at the new paths**

In `README.md` around line 136, the sentence ending "lives in
[`examples/basic`](examples/basic)" was changed to `examples/blog` in Task 1.
Append to that paragraph:

```markdown
[`examples/quickstart`](examples/quickstart) is the same idea in one package
with hand-written bindings; [`examples/README.md`](examples/README.md) compares
the two.
```

- [ ] **Step 3: Run the whole gate, every module**

Run: `cd "$(git rev-parse --show-toplevel)" && sh scripts/gate.sh`
Expected: `all modules pass` (`compare` may report skipped, which is normal
without its generated fixtures).

`go test ./...` reaches one module and this repository has four. Do not
substitute it here.

- [ ] **Step 4: Confirm generated code is current, exactly as CI does**

```bash
cd examples/blog && go generate ./...
cd "$(git rev-parse --show-toplevel)"
git diff --quiet && echo "generated code is current" || { echo "STALE:"; git diff --stat; }
```
Expected: `generated code is current`.

Note: the working tree carries unrelated modifications in `context.go`,
`exec.go`, `plan.go`, `subscription.go`, `loader/` and `ext/otel/`. If
`git diff` reports only those, the generated code is current — scope the check
with `git diff --quiet -- examples/` instead.

- [ ] **Step 5: Serve all three for real**

This is the step the others cannot replace. Three mains that compile against one
schema prove nothing about serving it.

For each of `./examples/blog/cmd/server`, `./examples/echo`, `./examples/fiber`:

```bash
go run <target> -addr :8099 &
sleep 2
curl -s localhost:8099/graphql -H 'content-type: application/json' \
  -d '{"query":"{ users { name posts { title } } }"}'
echo
curl -sN localhost:8099/graphql/stream -H 'content-type: application/json' \
  -H 'accept: text/event-stream' \
  -d '{"query":"subscription { postCreated { title } }"}' &
sleep 1
curl -s localhost:8099/graphql -H 'content-type: application/json' \
  -d '{"query":"mutation { createPost(authorId:\"1\", title:\"Live\", body:\"b\") { id } }"}'
sleep 1
kill %1 %2 2>/dev/null
```

Expected from every one of the three: the query returns
`{"data":{"users":[{"name":"ALICE",...`  (note `ALICE` — the `@upper` directive
is wired), and the stream prints `event: next` carrying
`{"data":{"postCreated":{"title":"Live"}}}`.

If Echo or Fiber answers the query but never delivers the subscription event,
the transport is wired but the schema's subscription is not reaching it — report
that rather than moving on.

- [ ] **Step 6: Commit**

```bash
cd "$(git rev-parse --show-toplevel)"
git add examples/README.md README.md
git commit -m "$(cat <<'EOF'
docs: add an examples README

Says which example to read first, what each layer in blog holds, and that
the domain/model separation is a demonstration rather than something the
library requires -- including what it costs on Post.author.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Wsx2gLrrQkLMK744k1gz2n
EOF
)"
```

---

## Notes for the executor

**On the `app` layer.** The spec measured a prototype at five of seven methods
forwarding. `blog` has nine methods and six of them forward. Do not treat that
as a defect to engineer away, and do not quote a hard number in the README — it
rots. If `app` ever reaches *all* forwarding, the spec's instruction is to drop
the layer and let `internal/transport/graphql` call the repository directly.

**On error strings.** `author %q does not exist` and `title cannot be cleared`
are asserted verbatim by tests moved in Task 6. `post %q does not exist` appears
in both `app.UpdatePost` and `Resolver.PostAuthor`; the latter is unreachable in
practice and exists so a deleted post cannot produce a nil dereference.

**Do not add what the spec excluded.** No `Dockerfile`, no `Makefile`, no
`config/` package. All three were considered and rejected: the first two would
restate `go generate` with nothing in CI building them, and an in-memory blog has
nothing to configure. The `-addr` flag belongs in each `main.go`. If one seems
necessary while implementing, that is a spec change — raise it, do not add it.

**On the frozen expectations.** Tasks 1 and 6 both end with the full 12 tests
green and no expected-response string edited. That is the whole argument that
this refactor preserved behaviour. If you find yourself editing one, the
refactor broke something — stop and say so.
