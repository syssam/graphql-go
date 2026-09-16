package schema

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/syssam/graphql-go"
	"github.com/syssam/graphql-go/examples/blog/graph/model"
)

func newExecutor(t *testing.T) *graphql.Executor {
	t.Helper()
	s, err := NewSchema(NewStore())
	if err != nil {
		t.Fatal(err)
	}
	return graphql.NewExecutor(s)
}

func execute(t *testing.T, e *graphql.Executor, query, variables string) string {
	t.Helper()
	req := &graphql.Request{Query: query}
	if variables != "" {
		req.Variables = json.RawMessage(variables)
	}
	resp := e.Execute(context.Background(), req)
	out, err := resp.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	resp.Release()
	return string(out)
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("response mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestNodeThroughInterface(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `{ a: node(id: "1") { id __typename ... on User { email } } b: node(id: "10") { ... on Post { title } } c: node(id: "x") { id } }`, "")
	expect(t, got, `{"data":{"a":{"id":"1","__typename":"User","email":"alice@example.com"},"b":{"title":"Hello"},"c":null}}`)
}

func TestNestedPostsWithArguments(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `query($n: Int) { user(id: "1") { name role createdAt posts(first: $n) { title tags publishedAt } } }`, `{"n": 1}`)
	expect(t, got, `{"data":{"user":{"name":"ALICE","role":"ADMIN","createdAt":"2026-01-01T00:00:00Z","posts":[{"title":"Hello","tags":["intro"],"publishedAt":"2026-01-03T00:00:00Z"}]}}}`)

	got = execute(t, e, `{ user(id: "1") { posts { title } } }`, "")
	expect(t, got, `{"data":{"user":{"posts":[{"title":"Hello"},{"title":"Go generics"}]}}}`)
}

func TestFilterInput(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `query($f: PostFilter) { posts(filter: $f) { id author { name } } }`, `{"f": {"tag": "go"}}`)
	expect(t, got, `{"data":{"posts":[{"id":"11","author":{"name":"ALICE"}}]}}`)

	got = execute(t, e, `{ posts(filter: {authorId: "2"}) { id } }`, "")
	expect(t, got, `{"data":{"posts":[{"id":"12"}]}}`)
}

func TestUnionSearch(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `{ search(term: "o") { __typename ... on User { name } ... on Post { title } } }`, "")
	expect(t, got, `{"data":{"search":[{"__typename":"User","name":"BOB"},{"__typename":"Post","title":"Hello"},{"__typename":"Post","title":"Go generics"}]}}`)
}

func TestUpdatePostDistinguishesAbsentFromNull(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `mutation { updatePost(id: "12", input: {title: "Final"}) { title body } }`, "")
	expect(t, got, `{"data":{"updatePost":{"title":"Final","body":"Work in progress"}}}`)

	got = execute(t, e, `mutation($in: UpdatePostInput!) { updatePost(id: "12", input: $in) { title body } }`, `{"in": {"body": null}}`)
	expect(t, got, `{"data":{"updatePost":{"title":"Final","body":""}}}`)

	got = execute(t, e, `mutation { updatePost(id: "12", input: {title: null}) { title } }`, "")
	if !strings.Contains(got, `"message":"title cannot be cleared"`) || !strings.Contains(got, `"data":null`) {
		t.Fatalf("got %s", got)
	}
}

func TestCreatePostAndErrors(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `mutation { createPost(authorId: "2", title: "New", body: "Body") { id title author { id } publishedAt } }`, "")
	expect(t, got, `{"data":{"createPost":{"id":"13","title":"New","author":{"id":"2"},"publishedAt":null}}}`)

	got = execute(t, e, `mutation { createPost(authorId: "nope", title: "New", body: "Body") { id } }`, "")
	expect(t, got, `{"errors":[{"message":"author \"nope\" does not exist","locations":[{"line":1,"column":12}],"path":["createPost"]}],"data":null}`)

	got = execute(t, e, `mutation { createPost(title: "New", body: "Body") { id } }`, "")
	if !strings.Contains(got, `"code":"GRAPHQL_VALIDATION_FAILED"`) {
		t.Fatalf("got %s", got)
	}
}

func TestIntrospectionSanity(t *testing.T) {
	e := newExecutor(t)
	got := execute(t, e, `{ __schema { description queryType { name } mutationType { name } } __type(name: "Time") { kind specifiedByURL } }`, "")
	expect(t, got, `{"data":{"__schema":{"description":"A small blog: users write posts.","queryType":{"name":"Query"},"mutationType":{"name":"Mutation"}},"__type":{"kind":"SCALAR","specifiedByURL":"https://tools.ietf.org/html/rfc3339"}}}`)
}

func TestNonNullBubbling(t *testing.T) {
	store := NewStore()
	store.posts = append(store.posts, &model.Post{ID: "99", Title: "Orphan", Tags: []string{}})
	store.postAuthor["99"] = "missing"
	s, err := NewSchema(store)
	if err != nil {
		t.Fatal(err)
	}
	e := graphql.NewExecutor(s)
	// author is non-null and posts elements are non-null, so the failure
	// propagates to the nullable user field.
	got := execute(t, e, `{ posts(filter: {authorId: "missing"}) { title author { name } } }`, "")
	expect(t, got, `{"errors":[{"message":"author \"missing\" of post \"99\" is missing","locations":[{"line":1,"column":48}],"path":["posts",0,"author"]}],"data":null}`)
}
