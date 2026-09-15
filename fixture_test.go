package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureSDL = `
interface Node { id: ID! }
interface Pet { name: String! }
type Dog implements Pet { name: String! barks: Boolean! }
type Cat implements Pet { name: String! lives: Int! }
type User implements Node {
  id: ID!
  name: String!
  nick: String
  tags: [String!]!
  scores: [Int]!
  friends: [User!]!
  bestFriend: User
  pet: Pet
  fail: String!
  failNullable: String
  bigInt: Int!
  role: Role!
}
type Post implements Node { id: ID! title: String! author: User! }
union SearchResult = User | Post
enum Role { ADMIN USER }
input Filter { name: String, limit: Int = 10 }
type Query {
  me: User!
  user(id: ID!): User
  users(filter: Filter): [User!]!
  usersSeq: [User!]!
  maybeUsers: [User]
  node(id: ID!): Node
  search(term: String!): [SearchResult!]!
  searchSeq(term: String!): [SearchResult!]!
  fail: String!
  failNullable: String
  panics: Int
  slow(ms: Int!): Int!
  strings: [String!]!
  nullableStrings: [String]!
  echo(v: Int, s: String = "d", f: Float, b: Boolean, list: [Int!]): String!
  ctxPath: String!
  selection: [String!]!
}
type Mutation { inc: Int! reset: Boolean! }
`

type fUser struct {
	ID      string
	Name    string
	Nick    *string
	Tags    []string
	Scores  []*int
	Friends []string
	Pet     fPet
	Role    fRole
}

type fPost struct {
	ID     string
	Title  string
	Author string
}

type fPet interface{ petName() string }

type fDog struct{ Name string }
type fCat struct{ Name string }

func (d *fDog) petName() string { return d.Name }
func (c *fCat) petName() string { return c.Name }

type fRole int

const (
	fRoleAdmin fRole = iota
	fRoleUser
)

type filterArgs struct {
	Name  *string
	Limit *int
}

type usersArgs struct{ Filter *filterArgs }
type idArgs struct{ ID string }
type termArgs struct{ Term string }
type slowArgs struct{ Ms int }
type echoArgs struct {
	V    *int
	S    *string
	F    *float64
	B    *bool
	List []int
}

type fixture struct {
	users   map[string]*fUser
	posts   map[string]*fPost
	counter atomic.Int64
	// calls records resolver invocations for ordering assertions.
	calls chan string
}

var errBoom = errors.New("boom")

func newFixture() *fixture {
	nick := "al"
	f := &fixture{
		users: map[string]*fUser{
			"1": {ID: "1", Name: "Alice", Nick: &nick, Tags: []string{"a", "b"}, Scores: []*int{ptr(1), nil, ptr(3)}, Friends: []string{"2", "3"}, Pet: &fDog{Name: "Rex"}, Role: fRoleAdmin},
			"2": {ID: "2", Name: "Bob", Tags: []string{}, Scores: []*int{}, Friends: []string{"1"}, Pet: &fCat{Name: "Tom"}, Role: fRoleUser},
			"3": {ID: "3", Name: "Carol", Tags: []string{"c"}, Scores: []*int{ptr(5)}, Friends: nil, Role: fRoleUser},
		},
		posts: map[string]*fPost{
			"p1": {ID: "p1", Title: "Hello", Author: "1"},
		},
		calls: make(chan string, 64),
	}
	return f
}

func (f *fixture) user(id string) *fUser { return f.users[id] }

func (f *fixture) options() []SchemaOption {
	return []SchemaOption{
		Enum("Role", map[fRole]string{fRoleAdmin: "ADMIN", fRoleUser: "USER"}),
		Interface[fPet]("Pet"),
		Object[fDog]("Dog",
			Field("name", func(d *fDog) string { return d.Name }),
			Field("barks", func(*fDog) bool { return true }),
		),
		Object[fCat]("Cat",
			Field("name", func(c *fCat) string { return c.Name }),
			Field("lives", func(*fCat) int { return 9 }),
		),
		Object[fUser]("User",
			Field("id", func(u *fUser) ID { return ID(u.ID) }),
			Field("name", func(u *fUser) string { return u.Name }),
			Field("nick", func(u *fUser) *string { return u.Nick }),
			Field("tags", func(u *fUser) []string { return u.Tags }),
			Field("scores", func(u *fUser) []*int { return u.Scores }),
			Resolve("friends", func(_ context.Context, u *fUser) ([]*fUser, error) {
				out := make([]*fUser, 0, len(u.Friends))
				for _, id := range u.Friends {
					out = append(out, f.users[id])
				}
				return out, nil
			}),
			Resolve("bestFriend", func(_ context.Context, u *fUser) (*fUser, error) {
				if len(u.Friends) == 0 {
					return nil, nil
				}
				return f.users[u.Friends[0]], nil
			}),
			Field("pet", func(u *fUser) fPet { return u.Pet }),
			Resolve("fail", func(context.Context, *fUser) (string, error) { return "", errBoom }),
			Resolve("failNullable", func(context.Context, *fUser) (*string, error) { return nil, errBoom }),
			Field("bigInt", func(*fUser) int64 { return 1 << 40 }),
			Field("role", func(u *fUser) fRole { return u.Role }),
		),
		Object[fPost]("Post",
			Field("id", func(p *fPost) string { return p.ID }),
			Field("title", func(p *fPost) string { return p.Title }),
			Field("author", func(p *fPost) *fUser { return f.users[p.Author] }),
		),
		Union[any]("SearchResult"),
		Input[filterArgs]("Filter",
			InputField("name", func(a *filterArgs, v *string) { a.Name = v }),
			InputField("limit", func(a *filterArgs, v *int) { a.Limit = v }),
		),
		Args[usersArgs](InputField("filter", func(a *usersArgs, v *filterArgs) { a.Filter = v })),
		Args[idArgs](InputField("id", func(a *idArgs, v string) { a.ID = v })),
		Args[termArgs](InputField("term", func(a *termArgs, v string) { a.Term = v })),
		Args[slowArgs](InputField("ms", func(a *slowArgs, v int) { a.Ms = v })),
		Args[echoArgs](
			InputField("v", func(a *echoArgs, v *int) { a.V = v }),
			InputField("s", func(a *echoArgs, v *string) { a.S = v }),
			InputField("f", func(a *echoArgs, v *float64) { a.F = v }),
			InputField("b", func(a *echoArgs, v *bool) { a.B = v }),
			InputField("list", func(a *echoArgs, v []int) { a.List = v }),
		),
		Object[Root]("Query",
			Resolve("me", func(context.Context, Root) (*fUser, error) { return f.users["1"], nil }),
			ResolveArgs("user", func(_ context.Context, _ Root, a idArgs) (*fUser, error) { return f.users[a.ID], nil }),
			ResolveArgs("users", func(_ context.Context, _ Root, a usersArgs) ([]*fUser, error) {
				ids := []string{"1", "2", "3"}
				var out []*fUser
				for _, id := range ids {
					u := f.users[id]
					if a.Filter != nil && a.Filter.Name != nil && u.Name != *a.Filter.Name {
						continue
					}
					out = append(out, u)
				}
				if a.Filter != nil && a.Filter.Limit != nil && len(out) > *a.Filter.Limit {
					out = out[:*a.Filter.Limit]
				}
				return out, nil
			}),
			Resolve("usersSeq", func(context.Context, Root) (iter.Seq[*fUser], error) {
				ids := []string{"1", "2", "3"}
				return func(yield func(*fUser) bool) {
					for _, id := range ids {
						if !yield(f.users[id]) {
							return
						}
					}
				}, nil
			}),
			Resolve("maybeUsers", func(context.Context, Root) ([]*fUser, error) { return []*fUser{f.users["1"], nil}, nil }),
			ResolveArgs("node", func(_ context.Context, _ Root, a idArgs) (any, error) {
				if u := f.users[a.ID]; u != nil {
					return u, nil
				}
				if p := f.posts[a.ID]; p != nil {
					return p, nil
				}
				return nil, nil
			}),
			ResolveArgs("search", func(_ context.Context, _ Root, a termArgs) ([]any, error) {
				return []any{f.users["1"], f.posts["p1"]}, nil
			}),
			ResolveArgs("searchSeq", func(_ context.Context, _ Root, a termArgs) (iter.Seq[any], error) {
				hits := []any{f.users["1"], f.posts["p1"]}
				return func(yield func(any) bool) {
					for _, h := range hits {
						if !yield(h) {
							return
						}
					}
				}, nil
			}),
			Resolve("fail", func(context.Context, Root) (string, error) { return "", errBoom }),
			Resolve("failNullable", func(context.Context, Root) (*string, error) { return nil, errBoom }),
			Resolve("panics", func(context.Context, Root) (*int, error) { panic("kaboom") }),
			ResolveArgs("slow", func(ctx context.Context, _ Root, a slowArgs) (int, error) {
				select {
				case <-time.After(time.Duration(a.Ms) * time.Millisecond):
				case <-ctx.Done():
					return 0, ctx.Err()
				}
				return a.Ms, nil
			}),
			Field("strings", func(Root) []string { return []string{"x", "y"} }),
			Field("nullableStrings", func(Root) []*string { return []*string{ptr("x"), nil} }),
			ResolveArgs("echo", func(_ context.Context, _ Root, a echoArgs) (string, error) {
				b, _ := json.Marshal(a)
				return string(b), nil
			}),
			Resolve("ctxPath", func(ctx context.Context, _ Root) (string, error) { return PathFrom(ctx).String(), nil }),
			Resolve("selection", func(ctx context.Context, _ Root) ([]string, error) {
				out := []string{}
				for sf := range SelectionFrom(ctx).Fields() {
					out = append(out, sf.Name)
				}
				return out, nil
			}),
		),
		Object[Root]("Mutation",
			Resolve("inc", func(context.Context, Root) (int, error) {
				n := f.counter.Add(1)
				time.Sleep(2 * time.Millisecond)
				f.calls <- "inc" + strconv.FormatInt(n, 10)
				return int(n), nil
			}),
			Resolve("reset", func(context.Context, Root) (bool, error) {
				f.counter.Store(0)
				return true, nil
			}),
		),
	}
}

func newFixtureExecutor(t testing.TB, opts ...ExecutorOption) (*fixture, *Executor) {
	t.Helper()
	f := newFixture()
	s, err := NewSchema(SDL(fixtureSDL), f.options()...)
	if err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	return f, NewExecutor(s, opts...)
}

// run executes a query with optional JSON variables.
func run(t testing.TB, e *Executor, query string, vars string) *Response {
	t.Helper()
	req := &Request{Query: query}
	if vars != "" {
		req.Variables = json.RawMessage(vars)
	}
	return e.Execute(context.Background(), req)
}

// expectData asserts the exact data payload and no errors.
func expectData(t testing.TB, resp *Response, want string) {
	t.Helper()
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %s", errorsJSON(resp.Errors))
	}
	if got := string(resp.Data); got != want {
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

func errorsJSON(errs []*Error) string {
	b, _ := json.Marshal(errs)
	return string(b)
}

// expectError asserts data and exactly one error with the given path.
func expectError(t *testing.T, resp *Response, wantData string, wantPath string, wantMsg string) {
	t.Helper()
	if got := string(resp.Data); got != wantData {
		t.Fatalf("data mismatch\n got: %s\nwant: %s\nerrors: %s", got, wantData, errorsJSON(resp.Errors))
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("want 1 error, got %d: %s", len(resp.Errors), errorsJSON(resp.Errors))
	}
	e := resp.Errors[0]
	if e.Path.String() != wantPath {
		t.Fatalf("path = %q, want %q (%s)", e.Path.String(), wantPath, errorsJSON(resp.Errors))
	}
	if wantMsg != "" && e.Message != wantMsg {
		t.Fatalf("message = %q, want %q", e.Message, wantMsg)
	}
	if len(e.Locations) == 0 {
		t.Fatalf("error has no location: %s", errorsJSON(resp.Errors))
	}
}

func fmtInts(v []int) string { return fmt.Sprint(v) }
