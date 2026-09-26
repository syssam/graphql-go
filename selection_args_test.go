package graphql

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// An ORM planning its loads from a parent's selection needs each child
// field's arguments before that child resolves, and it cannot name the
// generated argument struct. Real clients send first/where as variables, so
// SelectedField.Args, which is nil for those, answers the rare case.
const selectionArgsSDL = `
type Query { feed: Feed! }
input PostWhere { titleContains: String }
type Feed { posts(first: Int = 10, after: String, where: PostWhere): [Post!]! }
type Post { id: ID! title: String! }
`

type selFeed struct{}
type selPost struct{ ID, Title string }
type selWhere struct{ TitleContains *string }
type selPostsArgs struct {
	First *int
	After *string
	Where *selWhere
}

func captureFeedSelection(t *testing.T, query string, vars map[string]any) (Selection, map[string]any) {
	t.Helper()
	s, err := NewSchema(SDL(selectionArgsSDL),
		Object[Root]("Query", Resolve("feed", func(context.Context, Root) (*selFeed, error) { return &selFeed{}, nil })),
		Object[selFeed]("Feed", ResolveArgs("posts", func(context.Context, *selFeed, selPostsArgs) ([]selPost, error) {
			return []selPost{{ID: "1", Title: "t"}}, nil
		})),
		Object[selPost]("Post",
			Field("id", func(p *selPost) ID { return ID(p.ID) }),
			Field("title", func(p *selPost) string { return p.Title }),
		),
		Input[selWhere]("PostWhere",
			InputField("titleContains", func(w *selWhere, v *string) { w.TitleContains = v }),
		),
		Args[selPostsArgs](
			InputField("first", func(a *selPostsArgs, v *int) { a.First = v }),
			InputField("after", func(a *selPostsArgs, v *string) { a.After = v }),
			InputField("where", func(a *selPostsArgs, v *selWhere) { a.Where = v }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	var (
		sel     Selection
		opVars  map[string]any
		touched bool
	)
	e := NewExecutor(s, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			if fc.Field.Name == "feed" {
				sel, opVars, touched = fc.Selection(), OperationFrom(ctx).Variables, true
			}
			return next(ctx)
		})))
	raw, err := json.Marshal(vars)
	if err != nil {
		t.Fatal(err)
	}
	resp := e.Execute(context.Background(), &Request{Query: query, Variables: raw})
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	if !touched {
		t.Fatal("feed was never intercepted")
	}
	return sel, opVars
}

func selectedByAlias(t *testing.T, s Selection) map[string]SelectedField {
	t.Helper()
	out := map[string]SelectedField{}
	for f := range s.Fields() {
		out[f.Alias] = f
	}
	return out
}

func TestSelectedFieldArgumentMapReadsVariablesAndDefaults(t *testing.T) {
	sel, vars := captureFeedSelection(t, `query Q($n: Int, $w: PostWhere, $cur: String) {
		feed {
			a: posts(first: $n, where: $w, after: $cur) { id }
			b: posts { title }
			c: posts(first: 3, where: {titleContains: "go"}) { id title }
		}
	}`, map[string]any{"n": 2, "w": map[string]any{"titleContains": "x"}})

	got := selectedByAlias(t, sel)
	if len(got) != 3 {
		t.Fatalf("want the three aliases of posts, got %v", got)
	}
	for alias, want := range map[string]map[string]any{
		// $cur is not supplied: an absent variable is an absent argument,
		// not a null one, so a caller can tell "no cursor" from "cursor null".
		"a": {"first": json.Number("2"), "where": map[string]any{"titleContains": "x"}},
		"b": {"first": json.Number("10")},
		"c": {"first": json.Number("3"), "where": map[string]any{"titleContains": "go"}},
	} {
		f := got[alias]
		if f.Name != "posts" {
			t.Errorf("%s: name %q", alias, f.Name)
		}
		m, err := f.ArgumentMap(vars)
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("%s: ArgumentMap = %#v, want %#v", alias, m, want)
		}
	}
	if got["a"].Args != nil {
		t.Errorf("a's arguments use variables, so the plan cannot have decoded them; Args = %#v", got["a"].Args)
	}
}

func TestSelectedFieldSelectionIsPerAlias(t *testing.T) {
	sel, _ := captureFeedSelection(t, `{ feed { a: posts { id } b: posts { title } } }`, nil)
	got := selectedByAlias(t, sel)
	a, b := got["a"].Selection(), got["b"].Selection()
	if !a.Has("id") || a.Has("title") {
		t.Errorf("a selects only id; got id=%v title=%v", a.Has("id"), a.Has("title"))
	}
	if !b.Has("title") || b.Has("id") {
		t.Errorf("b selects only title; got id=%v title=%v", b.Has("id"), b.Has("title"))
	}
	// Sub answers by field name and so sees one alias; this is what the
	// per-field Selection is for.
	if sub, _ := sel.Sub("posts"); sub.Has("id") == sub.Has("title") {
		t.Errorf("Sub(posts) should be exactly one alias's selection")
	}
	for f := range a.Fields() {
		if !f.Selection().IsEmpty() {
			t.Errorf("leaf %s has a sub-selection", f.Name)
		}
		if m, err := f.ArgumentMap(nil); m != nil || err != nil {
			t.Errorf("leaf %s without arguments: ArgumentMap = %v, %v", f.Name, m, err)
		}
	}
}

func TestSelectionForTypeSplitsAnAbstractSelection(t *testing.T) {
	var sel Selection
	s, err := NewSchema(SDL(fixtureSDL), newFixture().options()...)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(s, WithFieldInterceptor(FieldInterceptorFunc(
		func(ctx context.Context, fc *FieldContext, next FieldHandler) (any, error) {
			if fc.Field.Name == "search" {
				sel = fc.Selection()
			}
			return next(ctx)
		})))
	run(t, e, `{ search(term: "a") {
		__typename
		... on User { name }
		... on Post { title }
		... on Node { id }
	} }`, "")

	user, post := sel.ForType("User"), sel.ForType("Post")
	for name, want := range map[string]bool{"__typename": true, "name": true, "id": true, "title": false} {
		if user.Has(name) != want {
			t.Errorf("User: Has(%q) = %v, want %v", name, !want, want)
		}
	}
	for name, want := range map[string]bool{"__typename": true, "title": true, "id": true, "name": false} {
		if post.Has(name) != want {
			t.Errorf("Post: Has(%q) = %v, want %v", name, !want, want)
		}
	}
	if !sel.ForType("Dog").IsEmpty() {
		t.Error("Dog is not a possible type of SearchResult, so nothing applies to it")
	}
	// A concrete selection is its own and only type's.
	if !user.ForType("anything").Has("name") {
		t.Error("ForType on a concrete selection must return it unchanged")
	}
}
